# Rate-Limit Service — Deep Design Notes

## Where It Lives: Both Planes

The rate-limit system is split across both planes. This is not a "data plane only" component and not a "control plane only" component — it's one of the few that straddles the boundary with clearly separated responsibilities.

### Control Plane — Policy & Configuration

The control plane owns the **definition** of rate-limit rules:

- **Per-connector rate-limit models**: What kind of limiter does this connector need? Token bucket, concurrency semaphore, composite? What are the numeric limits? This is connector metadata, stored in the Schema Catalog alongside the connector's capability declaration.
- **Per-tenant overrides**: ACME Corp has GitHub Enterprise with 15k requests/hour instead of the default 5k. This override lives in the Tenant & Connector Registry.
- **Per-user fairness quotas**: No single user can consume more than X% of their tenant's connector budget. This is tenant policy, stored alongside RLS/CLS rules in the Policy Store.
- **Async overflow policy**: When rate limits are exhausted, should the query be rejected (429) or routed to an async job queue? This is a per-tenant, per-connector config decision.

The control plane **never enforces** rate limits at runtime. It publishes configuration that the data plane reads.

### Data Plane — Enforcement & State

The data plane owns the **runtime enforcement** via a distributed rate limiter backed by Redis. At our scale target (~1k QPS), a Redis roundtrip per check is well within budget (~0.5ms p99 on the same AZ).

- **Limiter instances**: Token buckets, semaphores, composite limiters — one per unique key, state held in Redis so all gateway pods share a single view of remaining budget.
- **Allow/Deny decisions**: The executor calls `Allow()` before every connector fetch.
- **Retry-After computation**: When denied, computes the `Retry-After` header value.
- **Async routing**: When denied and overflow policy says "async," enqueues the query to the job queue.

### The Communication Pattern

```
Control Plane                              Data Plane
┌───────────────────┐                      ┌──────────────────────┐
│  Rate-Limit       │   config reads       │  Rate-Limit          │
│  Policy Store     │ ───────────────────► │  Service             │
│                   │   (cached, 30s TTL)  │                      │
│  • Connector      │                      │  • Limiter Pool      │
│    rate models    │                      │    (in-memory/Redis) │
│  • Tenant         │                      │  • Allow() on        │
│    overrides      │                      │    hot path          │
│  • User quotas    │                      │  • Retry-After       │
│  • Async policy   │                      │    computation       │
└───────────────────┘                      └──────────────────────┘
```

The data plane caches control plane config with a 30s TTL. If the control plane is briefly unavailable, enforcement continues with last-known config — never fails open.

---

## The Four Levels of Rate Limiting

A single token bucket keyed on `(tenant, connector)` is insufficient for a multi-tenant system. Rate limits are enforced at four levels, each protecting against a different failure mode.

### Level 1: Tenant Global

```
Key:      tenant_id
Limiter:  Token bucket
```

**What it protects against**: A single tenant's total query volume overwhelming shared infrastructure. Even if individual connector budgets are fine, a tenant sending 10k queries/second across 50 connectors saturates CPU, memory, and network on the gateway nodes.

**Example**: Tenant ACME runs a batch job that fires 5,000 queries in 10 seconds across GitHub, Jira, Salesforce, Slack, and Notion. Each connector's per-tenant budget might allow it, but the aggregate load is unacceptable for shared infrastructure.

**Configuration**:
```yaml
tenant_global:
  default:
    requests_per_second: 100
    burst: 200
  overrides:
    acme-corp:
      requests_per_second: 500    # enterprise tier
      burst: 1000
```

**When this fires**: Before the query is even parsed. The gateway checks L1 immediately after AuthN. If the tenant is over budget globally, there's no point parsing SQL or consulting the planner.

### Level 2: User Within Tenant

```
Key:      tenant_id:user_id
Limiter:  Token bucket
```

**What it protects against**: A single user exhausting their tenant's budget, starving all other users in the same organization. This is the "noisy neighbor within a tenant" problem.

**Example**: Developer Alice at ACME Corp writes a script that loops `SELECT * FROM github.pull_requests` every 100ms. Without L2, Alice burns ACME's entire GitHub API budget. The other 500 developers at ACME get `RATE_LIMIT_EXHAUSTED` on their next query — through no fault of their own.

**Configuration**:
```yaml
user_global:
  default:
    requests_per_second: 20
    burst: 50
  # User quotas can also be expressed as a percentage of tenant budget:
  # max_tenant_share: 0.20  → no single user can consume more than 20% of tenant budget
```

**When this fires**: After AuthN extracts the `Principal`, before query parsing. Cheap check, high impact.

### Level 3: Tenant × Connector

```
Key:      tenant_id:connector_id
Limiter:  Varies by connector (token bucket, semaphore, or composite)
```

**What it protects against**: Exhausting the SaaS API's rate limit for this tenant's credentials. This is the most critical level because it maps directly to an **external constraint** we don't control.

GitHub gives you 5,000 requests/hour per OAuth token. Jira Cloud gives you ~N concurrent requests. Salesforce gives you 100,000 API calls per day. If we exceed these, the SaaS vendor returns 429s and our system is blind to that source until the window resets.

**Why the limiter type varies**: Different SaaS APIs enforce rate limits in fundamentally different ways. A single limiter type cannot model all of them correctly. This is the connector capability intersection discussed in detail in the next section.

**Example**: ACME's GitHub OAuth token has a 5,000 req/hour budget. ACME has 500 active developers. If each developer sends 12 queries that hit GitHub in an hour, we've consumed 6,000 requests — over budget. L3 prevents this by enforcing the aggregate budget at the tenant level.

**Configuration**:
```yaml
tenant_connector:
  github:
    model: token_bucket
    requests_per_hour: 5000
    burst: 100
  jira:
    model: concurrency
    max_concurrent: 10
    requests_per_minute: 300
  overrides:
    acme-corp:
      github:
        requests_per_hour: 15000    # GitHub Enterprise plan
```

**When this fires**: At execution time, before the connector `Fetch()` call. After cache check — if cache hits, no rate-limit token is consumed.

### Level 4: User × Connector

```
Key:      tenant_id:user_id:connector_id
Limiter:  Same type as L3 (inherits from connector model), but with fractional budget
```

**What it protects against**: A single user burning all of their tenant's budget for a specific connector. L2 caps the user's total volume across all connectors, but L4 provides per-connector fairness.

**Example**: Developer Bob at ACME runs heavy GitHub analytics queries. L2 says Bob can do 20 req/s total — fine. But all 20 requests hit GitHub. Meanwhile, ACME's GitHub budget is 15k/hour. If Bob sustains 20 req/s to GitHub for an hour, that's 72,000 requests — far over the 15k budget. L4 limits Bob to, say, 20% of ACME's GitHub budget (3,000 req/hour).

**Configuration**:
```yaml
user_connector:
  default:
    max_tenant_share: 0.20    # each user gets at most 20% of tenant's connector budget
  overrides:
    acme-corp:
      alice:
        github:
          max_tenant_share: 0.50   # Alice is the GitHub admin, needs more headroom
```

**When this fires**: Same point as L3 — at execution time, before `Fetch()`. Checked after L3 passes (if the tenant is over the connector budget globally, no need to check per-user).

### Check Sequence

```
Query arrives → AuthN → extract Principal (tenant_id, user_id)

┌─────────────────────────────────────────────────────────┐
│ L1: Tenant Global                                       │
│ Key: tenant_id                                          │
│ Check: Has this tenant exceeded aggregate QPS?          │
│ Deny → 429 RATE_LIMIT_EXHAUSTED                        │
│        "Tenant query budget exhausted. Retry in Xs."    │
│        (no query parsing, no connector calls)           │
└────────────────────────┬────────────────────────────────┘
                         │ pass
                         ▼
┌─────────────────────────────────────────────────────────┐
│ L2: User Global                                         │
│ Key: tenant_id:user_id                                  │
│ Check: Has this user exceeded their personal QPS?       │
│ Deny → 429 RATE_LIMIT_EXHAUSTED                        │
│        "Personal query budget exhausted. Retry in Xs."  │
└────────────────────────┬────────────────────────────────┘
                         │ pass
                         ▼
               Parse SQL → Extract connector IDs
                         │
          ┌──────────────┼──────────────────┐
          ▼              ▼                  ▼
   ┌─────────────┐ ┌─────────────┐  ┌─────────────┐
   │ Connector A │ │ Connector B │  │ Connector C │
   │             │ │             │  │             │
   │ L3 check   │ │ L3 check   │  │ L3 check   │
   │ L4 check   │ │ L4 check   │  │ L4 check   │
   │ Cache check │ │ Cache check │  │ Cache check │
   │ Fetch()     │ │ Fetch()     │  │ Fetch()     │
   └─────────────┘ └─────────────┘  └─────────────┘
```

L1/L2 are checked **before** SQL parsing (cheap, short-circuit early). L3/L4 are checked **per connector** after the planner identifies which connectors are needed — a query touching only Jira shouldn't be denied because GitHub's budget is exhausted.

### Why Four Levels, Not Fewer?

The temptation is to collapse levels. Here's why each one is necessary:

| Without this level | Failure mode |
|---|---|
| No L1 (tenant global) | A tenant with 50 connectors sends moderate traffic to each — individually fine, but aggregate load crashes the gateway. |
| No L2 (user global) | One script-happy user at a 10,000-person tenant starves everyone. Tenant looks fine in aggregate; individual users are suffering. |
| No L3 (tenant×connector) | Tenant sends 100 req/s to GitHub across 500 users. Each user is within their L2 budget, but the tenant's OAuth token gets 429'd by GitHub. |
| No L4 (user×connector) | One user sends 80% of their queries to a single connector. L2 says the user is within total budget, L3 says the tenant's connector budget is fine (so far), but the user is on track to exhaust L3 single-handedly. |

---

## Connector Rate-Limit Models — The Sharp Edge

**Not all SaaS APIs enforce rate limits the same way.** A system that assumes "token bucket everywhere" will either over-restrict connectors that allow burst, or under-restrict connectors that don't.

### The Problem

A token bucket works when the API says "here's your budget, spend it however you want." But some APIs limit concurrent in-flight requests — a fundamentally different constraint.

### Real-World Rate-Limit Models

| Connector | API Rate-Limit Model | Burst Behavior |
|---|---|---|
| **GitHub** | 5,000 req/hour per token; header-based (`X-RateLimit-Remaining`) | Budget-based. Burst is fine. |
| **Jira Cloud** | Concurrency-based (max N concurrent) + secondary per-minute cap | Slot-based. No burst concept. |
| **Salesforce** | Daily API call limit (100k/day) + per-15s concurrent cap | Composite. Daily allows burst, concurrent cap doesn't. |
| **Google Workspace** | Per-user per-100-second quotas (rolling window) | Rolling window. No real burst. |
| **Zendesk** | Per-minute token bucket with fixed burst size | Token bucket, vendor-capped burst. |
| **Notion** | 3 requests/second per integration | Fixed rate. No burst. |
| **Slack** | Per-method rate limits (tier 1–4) | Method-specific limits. |

### Limiter Types

Multiple limiter implementations behind the same `Allow()` interface, selected based on the connector's declared model.

#### 1. Token Bucket — For Budget-Based Connectors

```
Use when:  The SaaS API gives you a budget for a time window and lets you spend it freely.
Examples:  GitHub (5k/hour), Zendesk (200/min with burst)
Behavior:  Allows burst up to configured size, then sustains at steady rate. Tokens refill over time.
```

The prototype uses Go's `golang.org/x/time/rate.Limiter`.

**Configuration per connector**:
```yaml
rate_limit:
  model: token_bucket
  rate: 83              # 5000/hour ≈ 83/min ≈ 1.38/sec
  burst: 100            # allow 100-request bursts
  window: 1h            # for human-readable budgeting
```

Burst is set conservatively below the vendor's limit to leave headroom for retries and background operations (token refresh, webhooks) that share the same API budget.

#### 2. Concurrency Semaphore — For Slot-Based Connectors

```
Use when:  The SaaS API limits how many requests can be in-flight simultaneously.
Examples:  Jira Cloud (10 concurrent), Salesforce (25 concurrent per 15s window)
Behavior:  Admits up to N concurrent requests. The (N+1)th blocks (with timeout) or is rejected.
```

Implemented as a bounded channel or `sync.Semaphore` — slots are **acquired** at request start and **released** on completion (unlike tokens which refill over time).

**Configuration per connector**:
```yaml
rate_limit:
  model: concurrency
  max_concurrent: 10
  acquire_timeout: 2s     # how long to wait for a slot before returning 429
```

**Why not a token bucket?** If Jira allows 10 concurrent requests and we burst 50, 40 get 429'd immediately. The semaphore correctly models the constraint: only 10 in-flight at any moment.

**Lifecycle**: `acquire slot → Fetch() → release slot (even on error)`. Release-on-error is critical — leaked slots shrink effective concurrency until the connector is fully blocked.

#### 3. Composite — For Connectors with Multiple Constraints

```
Use when:  The SaaS API enforces both a rate AND a concurrency limit.
Examples:  Salesforce (100k/day + 25 concurrent), Slack (tiered per-method + global)
Behavior:  Checks ALL sub-limiters. Most restrictive wins.
```

A wrapper holding multiple sub-limiters, evaluated in order with rollback — if any denies, all previously acquired ones are released:

```
CompositeAllow(ctx, key):
    for each sub-limiter:
        if sub-limiter.Deny():
            rollback all previously acquired sub-limiters
            return Deny(most restrictive Retry-After)
    return Allow
```

**Configuration per connector**:
```yaml
rate_limit:
  model: composite
  sub_limits:
    - type: token_bucket
      rate: 1157        # 100k/day ≈ 1157/min
      burst: 200
    - type: concurrency
      max_concurrent: 25
      acquire_timeout: 2s
```

### How the Connector SDK Declares Its Rate-Limit Model

The connector's capability declaration includes a `rate_limit` block:

```yaml
connector: github
version: "1.3.0"
tables:
  pull_requests:
    pushdown_ops: ["=", "!=", "in"]
    pushdown_columns: ["state", "author", "base_branch"]
    supports_etag: true
    supports_pagination: true
    max_page_size: 100
rate_limit:
  model: token_bucket
  requests_per_hour: 5000
  burst: 100
  headers:
    remaining: "X-RateLimit-Remaining"
    reset: "X-RateLimit-Reset"
    limit: "X-RateLimit-Limit"
```

```yaml
connector: jira
version: "2.1.0"
tables:
  issues:
    pushdown_ops: ["=", "!=", "in"]
    pushdown_columns: ["assignee", "status", "project", "priority"]
    supports_etag: true
rate_limit:
  model: concurrency
  max_concurrent: 10
  requests_per_minute: 300
  retry_after_header: "Retry-After"
```

The Rate-Limit Service reads these at startup (cached from the Schema Catalog) and creates the correct limiter type. New connectors register their rate-limit model automatically — no code changes needed.

### Adaptive Rate Limiting — Reading Response Headers

Some SaaS APIs report remaining budget via response headers. The connector SDK captures these and feeds them back:

```
Connector fetches from GitHub API
  Response headers:
    X-RateLimit-Remaining: 4200
    X-RateLimit-Reset: 1709164800 (Unix timestamp)

Connector passes this to Rate-Limit Service:
  UpdateBudget(tenantID, "github", remaining=4200, resetAt=...)
```

The service adjusts the token bucket's fill rate to match reality — if the header says 4,200 remaining with 45 minutes left, effective rate is 93/min. This matters because **we may not be the only client using this OAuth token.** The customer's other integrations share the same credentials; adaptive limiting trusts the source's accounting over our internal estimate.

Not all connectors support this (declared via the `headers` block). Without it, we rely on internal limiter state and treat the configured limit as a hard ceiling.

---

## Rate-Limit Service Architecture

```
┌─────────────────────────────────────────────────────────────────────────┐
│                         Rate-Limit Service                              │
│                                                                         │
│  ┌─────────────────────────────────────────────────────────────────┐   │
│  │  Policy Loader (from Control Plane, cached 30s TTL)             │   │
│  │                                                                 │   │
│  │  • Connector rate-limit models (token_bucket / concurrency /    │   │
│  │    composite)                                                   │   │
│  │  • Per-tenant overrides (enterprise plans, custom quotas)       │   │
│  │  • Per-user fairness quotas (max_tenant_share %)                │   │
│  │  • Async overflow policies                                      │   │
│  └─────────────────────────────────────────────────────────────────┘   │
│                                                                         │
│  ┌─────────────────────────────────────────────────────────────────┐   │
│  │  Limiter Pool (Redis-backed, distributed across gateway pods)   │   │
│  │                                                                 │   │
│  │  L1 buckets:  map[tenant_id]                    → TokenBucket   │   │
│  │  L2 buckets:  map[tenant_id:user_id]            → TokenBucket   │   │
│  │  L3 buckets:  map[tenant_id:connector_id]       → Limiter*      │   │
│  │  L4 buckets:  map[tenant_id:user_id:conn_id]    → Limiter*      │   │
│  │                                                                 │   │
│  │  * Limiter type varies by connector model                       │   │
│  │    (TokenBucket, Semaphore, or Composite)                       │   │
│  └─────────────────────────────────────────────────────────────────┘   │
│                                                                         │
│  ┌─────────────────────────────────────────────────────────────────┐   │
│  │  Public Interface                                               │   │
│  │                                                                 │   │
│  │  CheckGlobal(ctx, tenantID, userID)                             │   │
│  │    → checks L1, L2                                              │   │
│  │    → called at gateway, before query parsing                    │   │
│  │                                                                 │   │
│  │  CheckConnector(ctx, tenantID, userID, connectorID)             │   │
│  │    → checks L3, L4                                              │   │
│  │    → called at executor, before each connector Fetch()          │   │
│  │                                                                 │   │
│  │  Release(tenantID, userID, connectorID)                         │   │
│  │    → releases concurrency slots (for semaphore-based limiters)  │   │
│  │    → called after Fetch() completes (success or error)          │   │
│  │                                                                 │   │
│  │  UpdateBudget(tenantID, connectorID, remaining, resetAt)        │   │
│  │    → adaptive recalibration from response headers               │   │
│  │    → called by connector after each API response                │   │
│  └─────────────────────────────────────────────────────────────────┘   │
│                                                                         │
│  ┌─────────────────────────────────────────────────────────────────┐   │
│  │  Async Overflow Router                                          │   │
│  │                                                                 │   │
│  │  When L3/L4 deny and overflow policy = "async":                 │   │
│  │    → enqueue query to async job queue                           │   │
│  │    → return job_id + estimated_completion to client              │   │
│  │    → job executes when rate-limit budget refills                │   │
│  └─────────────────────────────────────────────────────────────────┘   │
└─────────────────────────────────────────────────────────────────────────┘
```

### The `Release()` Method

Concurrency-based limiters (Jira, Salesforce) need explicit slot release on completion. For token-bucket connectors, `Release()` is a no-op. The executor uses `defer` to guarantee slot return:

```go
if err := limiter.CheckConnector(ctx, tenantID, userID, connectorID); err != nil {
    return err
}
defer limiter.Release(tenantID, userID, connectorID)

rows, meta, err := connector.Fetch(ctx, principal, sourceQuery)
```

---

## Distributed State via Redis

All L1–L4 limiter state lives in Redis — every `Allow()` call is a Redis `EVAL` (atomic check + decrement). At ~1k QPS this adds ~0.5ms per check, well within our latency budget. Per-pod in-memory state is never authoritative; Redis is the single source of truth so that all gateway pods share one view of remaining budget.

`EVAL` with Lua scripts handles atomic token-bucket operations; `SETNX` with TTL provides distributed semaphore slots with auto-recovery on pod crash.

```
Key schema in Redis:
  rl:l3:{tenant_id}:{connector_id}        → token count + last_refill_at
  rl:l4:{tenant_id}:{user_id}:{conn_id}   → token count + last_refill_at
  rl:sem:{tenant_id}:{connector_id}        → set of active slot holders (with TTL)
```

> **Prototype shortcut**: The prototype uses an in-process `sync.RWMutex`-guarded map for simplicity. Production replaces this with Redis — the `Allow()` interface is unchanged.

### Failure Mode: Redis Down

Falls back to local in-process limiters with a conservative per-pod budget: `configured_limit / pod_count`. Intentionally pessimistic to avoid overshooting external limits. When Redis recovers, switches back seamlessly. The fallback is emitted as `ratelimit_redis_fallback_active{connector=...}`.

---

## Interaction with Cache Layer

Rate-limit checks and cache checks happen in a specific order:

```
Per connector fetch:
  1. L3 check (tenant×connector) ─┐
  2. L4 check (user×connector)  ──┤── rate-limit gate
                                   │
                        ┌──────────┘
                        │ pass
                        ▼
  3. Cache lookup (pushed predicates key)
     │
     ├── HIT:  return cached rows (no API call, no token consumed... wait)
     │
     └── MISS: call connector.Fetch() → consume external API budget
```

**Should a cache hit consume a rate-limit token?** No — the token represents an external API call that a cache hit avoids. But we check L3/L4 *before* the cache lookup.

**Resolution**: The check is a **reservation**, not a consumption. Cache hit → reservation cancelled (token returned). Cache miss → reservation committed.

```go
reservation := limiter.Reserve(tenantID, connectorID)

if cached, _, ok := cache.Get(cacheKey, maxStaleness); ok {
    reservation.Cancel()   // cache hit — return the token
    return cached, nil
}

// cache miss — reservation stands, proceed to live fetch
rows, meta, err := connector.Fetch(ctx, principal, sourceQuery)
```

For concurrency semaphores, the slot is acquired before cache check and released immediately on hit (microseconds hold time).

---

## Async Overflow Path

When L3/L4 denies a request, the default is a 429 with `Retry-After`. The async overflow path provides an alternative:

```
L3 denies → check async overflow policy for this tenant×connector

Policy = "reject":
  → return 429 RATE_LIMIT_EXHAUSTED
  → Retry-After: <seconds until budget refills>

Policy = "async":
  → enqueue query to async job queue (SQS / Redis Stream)
  → return 202 Accepted with:
    {
      "job_id": "abc123",
      "status": "QUEUED",
      "estimated_completion_ms": 45000,
      "poll_url": "/v1/jobs/abc123"
    }
  → job executes when rate-limit budget refills
  → client polls /v1/jobs/{id} or receives webhook notification
```

**Eligibility** depends on tenant policy (admin enables per connector), query type (partial async for JOINs where one connector is throttled), and client hint (`"allow_async": true`).

**Always synchronous 429**: Queries with `max_staleness: 0` (user wants live data) or from interactive UIs with short timeouts.

---

## Error Vocabulary

| HTTP / condition | Standard code | Meaning |
|---|---|---|
| 429 / rate-limit header | `RATE_LIMIT_EXHAUSTED` | API budget exhausted; includes `Retry-After` |
| 401 / token expired | `ENTITLEMENT_DENIED` | Credential invalid or expired |
| 403 / insufficient scope | `ENTITLEMENT_DENIED` | Auth OK but permission denied at source |
| Context deadline exceeded | `SOURCE_TIMEOUT` | Query timed out waiting on connector |
| HTTP 5xx / network error | `SOURCE_TIMEOUT` | Source unavailable; treat as transient |
| Schema mismatch / unknown table | `INVALID_QUERY` | Table or column does not exist |
| Freshness constraint unmet | `STALE_DATA` | Cache too old, live fetch rate-limited |

---

## Error Model

Rate-limit denials produce structured, actionable errors:

```json
{
  "error": {
    "code": "RATE_LIMIT_EXHAUSTED",
    "message": "GitHub API budget for tenant acme-corp exhausted. 4,200 of 5,000 hourly requests consumed.",
    "connector_id": "github",
    "level": "tenant_connector",
    "retry_after_seconds": 127,
    "budget": {
      "limit": 5000,
      "remaining": 0,
      "resets_at": "2026-02-28T15:00:00Z",
      "window": "1h"
    },
    "suggestion": "Retry after 127s, increase max_staleness to serve from cache, or contact your admin to upgrade the API plan.",
    "async_available": true,
    "trace_id": "4bf92f3577b34da6"
  }
}
```

Key fields:

- **`level`**: Which rate limit was hit (`tenant_global`, `user_global`, `tenant_connector`, `user_connector`) — different levels require different remediation.
- **`retry_after_seconds`**: Computed from the limiter's refill schedule (token refill time or estimated request duration for semaphores).
- **`budget`**: Full budget picture so callers can make informed decisions.
- **`suggestion`**: Human-readable guidance varying by level (L1/L2: reduce frequency; L3: retry/cache/upgrade; L4: share exhaustion notice).
- **`async_available`**: Whether the client can re-submit with `"allow_async": true`.

---

## Fairness Across Tenants — Head-of-Line Blocking Avoidance

In a multi-tenant deployment, a rate-limited tenant must not block others. The design avoids this through:

1. **Non-blocking checks**: `Allow()` never sleeps — returns immediately with allow or deny.
2. **Per-tenant limiter isolation**: Each tenant's limiter is a separate object. No cross-tenant mutex contention.
3. **Errgroup per query**: A rate-limited connector in one query doesn't affect another query's goroutines.
4. **Per-tenant queue partitions**: Async overflow uses per-tenant SQS message groups — 10k queued jobs for one tenant don't delay another's first job.

---

## Observability

### Prometheus Metrics

```
# Token bucket state
ratelimit_tokens_remaining{level, tenant_id, connector_id, user_id}  gauge
ratelimit_tokens_consumed_total{level, tenant_id, connector_id}      counter

# Decision outcomes
ratelimit_decisions_total{level, decision=allowed|denied, tenant_id, connector_id}  counter

# Latency of Allow() calls (should be <1ms)
ratelimit_check_duration_seconds{level}  histogram

# Redis state (production)
ratelimit_redis_fallback_active{connector_id}  gauge
ratelimit_redis_latency_seconds  histogram

# Async overflow
ratelimit_async_enqueued_total{tenant_id, connector_id}  counter
ratelimit_async_queue_depth{tenant_id, connector_id}  gauge
```

### OpenTelemetry Spans

Every rate-limit check adds a span to the request trace:

```
Span: ratelimit.check
  Attributes:
    ratelimit.level: "tenant_connector"
    ratelimit.decision: "allowed"
    ratelimit.remaining: 4200
    ratelimit.connector_id: "github"
    ratelimit.tenant_id: "acme-corp"
  Duration: 0.2ms
```

On denial, the span includes `retry_after_seconds` and `budget.*` attributes for correlation in Jaeger.

### Alerts

| Alert | Condition | Severity |
|---|---|---|
| `RateLimitBudgetLow` | Tenant's L3 budget < 10% remaining for any connector | Warning |
| `RateLimitBudgetExhausted` | Tenant's L3 budget = 0 and queries are being denied | Critical |
| `RateLimitRedisFallback` | Redis-backed limiter fell back to local state | Warning |
| `RateLimitAsyncQueueDepth` | Async overflow queue > 1,000 jobs for any tenant | Warning |
| `RateLimitSlotLeak` | Concurrency semaphore slots held > 5 minutes (probable leak) | Critical |

---

## Summary

| Design Decision | Rationale |
|---|---|
| **Four-level hierarchy** | Each level protects against a different failure mode. Collapsing any level leaves a gap. |
| **Connector-declared rate-limit model** | Burst is a connector property, not a system property. A universal token bucket is incorrect for slot-based APIs. |
| **Policy in control plane, enforcement in data plane** | Config changes don't require data plane redeployment. Enforcement is on the hot path — must be sub-millisecond. |
| **Redis-backed state for L1–L4** | Avoids the pod-count multiplier problem. L1/L2 may have longer sync intervals but still require global consistency. |
| **Reservation pattern with cache** | Cache hits shouldn't consume rate-limit tokens. Reservation is cancelled on hit, committed on miss. |
| **Async overflow as opt-in** | Interactive queries should fail fast; batch queries should queue gracefully. |
| **Adaptive recalibration from headers** | We may not be the only consumer of the API credentials. Source's own accounting is more accurate. |
