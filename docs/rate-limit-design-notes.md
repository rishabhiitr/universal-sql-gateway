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

The data plane owns the **runtime enforcement**:

- **Limiter instances**: Token buckets, semaphores, composite limiters — all live in-memory in the data plane, one per unique key.
- **Allow/Deny decisions**: The executor calls `Allow()` before every connector fetch. This is on the hot path — must be sub-millisecond.
- **State synchronization**: In multi-node deployments, limiter state is backed by Redis so that all gateway pods share the same view of remaining budget.
- **Retry-After computation**: When a request is denied, the data plane computes the `Retry-After` header value.
- **Async routing**: When a request is denied and the overflow policy says "async," the data plane enqueues the query to the job queue.

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

The data plane caches the control plane configuration locally with a short TTL (30s). This avoids synchronous lookups on the hot path. If the control plane is briefly unavailable, the data plane continues enforcing with the last-known config — rate-limit enforcement degrades gracefully, never fails open.

---

## The Four Levels of Rate Limiting

A single token bucket keyed on `(tenant, connector)` is insufficient for a multi-tenant system with thousands of users per tenant. Rate limits need to be enforced at four distinct levels, each protecting against a different failure mode.

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

The four levels are checked in order, short-circuiting on the first denial:

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

L1 and L2 are checked **before** SQL parsing — they're cheap, and if the tenant/user is over budget there's no point doing any work. L3 and L4 are checked **per connector** after the planner has identified which connectors are needed. This matters: a query touching only Jira shouldn't be denied because GitHub's budget is exhausted.

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

This is the most important and least obvious part of the design. **Not all SaaS APIs enforce rate limits the same way.** A system that assumes "token bucket everywhere" will either over-restrict connectors that allow burst, or under-restrict connectors that don't.

### The Problem

A token bucket with burst allows a client to send a burst of requests up front, then sustain a steady rate. This works when the SaaS API says "here's your budget for the hour, spend it however you want." But some APIs say "you can only have N requests in-flight at once" — that's a fundamentally different constraint.

### Real-World Rate-Limit Models

| Connector | API Rate-Limit Model | Burst Behavior |
|---|---|---|
| **GitHub** | 5,000 req/hour per token; header-based (`X-RateLimit-Remaining`) | Budget-based. You can spend 5,000 in the first minute if you want. Burst is fine. |
| **Jira Cloud** | Concurrency-based (max N concurrent requests per tenant) + secondary per-minute cap | Slot-based. Exceeding concurrent slots → immediate 429 or queuing. No burst concept. |
| **Salesforce** | Daily API call limit (e.g., 100k/day for Enterprise) + per-15s concurrent request cap | Composite. Daily budget allows burst within a day, but the 15-second concurrent cap doesn't. |
| **Google Workspace** | Per-user per-100-second quotas (rolling window) | Rolling window. No real burst — usage is smoothed over the window. |
| **Zendesk** | Per-minute token bucket with fixed burst size | Token bucket, but burst is capped by the vendor (e.g., burst of 10, sustained 200/min). |
| **Notion** | 3 requests/second per integration | Fixed rate. No burst. Exceeding → 429 immediately. |
| **Slack** | Per-method rate limits (tier 1–4) with different limits per API method | Method-specific. `conversations.list` has a different limit than `chat.postMessage`. |

### Limiter Types

The Rate-Limit Service needs multiple limiter implementations behind the same `Allow()` interface. The correct limiter is selected based on the connector's declared rate-limit model.

#### 1. Token Bucket — For Budget-Based Connectors

```
Use when:  The SaaS API gives you a budget for a time window and lets you spend it freely.
Examples:  GitHub (5k/hour), Zendesk (200/min with burst)
Behavior:  Allows burst up to the configured burst size, then sustains at the steady rate.
           Tokens refill over time.
```

The Go standard library's `golang.org/x/time/rate.Limiter` implements this correctly. The prototype already uses it.

**Configuration per connector**:
```yaml
rate_limit:
  model: token_bucket
  rate: 83              # 5000/hour ≈ 83/min ≈ 1.38/sec
  burst: 100            # allow 100-request bursts
  window: 1h            # for human-readable budgeting
```

**How burst works**: If the bucket is full (100 tokens), a sudden spike of 100 requests passes immediately. Then the system sustains at ~1.38 req/sec as tokens refill. The burst parameter is set conservatively below the vendor's limit to leave headroom for retries and background operations (token refresh, webhook registration, etc.) that also consume the same API budget.

#### 2. Concurrency Semaphore — For Slot-Based Connectors

```
Use when:  The SaaS API limits how many requests can be in-flight simultaneously.
Examples:  Jira Cloud (10 concurrent), Salesforce (25 concurrent per 15s window)
Behavior:  Admits up to N concurrent requests. The (N+1)th request blocks (with timeout)
           or is rejected.
```

Implemented as a bounded channel or `sync.Semaphore`. The key difference from a token bucket: tokens are not consumed and refilled over time — they're **acquired** when a request starts and **released** when it completes.

**Configuration per connector**:
```yaml
rate_limit:
  model: concurrency
  max_concurrent: 10
  acquire_timeout: 2s     # how long to wait for a slot before returning 429
```

**Why burst doesn't work here**: If Jira allows 10 concurrent requests and we send 50 in a burst, 40 of them get 429'd immediately. A token bucket with burst=50 would have allowed this. The concurrency semaphore correctly models the constraint: only 10 can be in-flight at any moment, regardless of how fast they arrive.

**Lifecycle**:
```
request arrives → acquire semaphore slot (or timeout → 429)
   → execute Fetch()
   → release semaphore slot (even on error)
```

The release-on-error is critical. If a connector fetch times out or returns an error, the slot must be freed. Otherwise leaked slots shrink the effective concurrency over time until the connector is fully blocked.

#### 3. Composite — For Connectors with Multiple Constraints

```
Use when:  The SaaS API enforces both a rate AND a concurrency limit (or a daily cap + a short-window limit).
Examples:  Salesforce (100k/day + 25 concurrent per 15s), Slack (tiered per-method + global)
Behavior:  Checks ALL sub-limiters. The most restrictive one wins. If any sub-limiter denies,
           the request is denied.
```

Implemented as a wrapper that holds multiple sub-limiters and evaluates them in order:

```
CompositeAllow(ctx, key):
    for each sub-limiter:
        if sub-limiter.Deny():
            rollback all previously acquired sub-limiters
            return Deny(most restrictive Retry-After)
    return Allow
```

**Rollback is important**: If the concurrency semaphore allows but the daily budget denies, we must release the semaphore slot. Otherwise the denied request still holds a concurrency slot it never used.

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

The connector's capability declaration (already described in the Connector SDK section of the design doc) includes a `rate_limit` block:

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

The Rate-Limit Service reads these declarations at startup (cached from the Schema Catalog) and creates the correct limiter type for each connector. When a new connector is onboarded, its rate-limit model is registered automatically — no code changes to the rate-limit service.

### Adaptive Rate Limiting — Reading Response Headers

Some SaaS APIs tell us exactly how much budget remains via response headers. The connector SDK captures these and feeds them back to the Rate-Limit Service:

```
Connector fetches from GitHub API
  Response headers:
    X-RateLimit-Remaining: 4200
    X-RateLimit-Reset: 1709164800 (Unix timestamp)

Connector passes this to Rate-Limit Service:
  UpdateBudget(tenantID, "github", remaining=4200, resetAt=...)
```

The Rate-Limit Service can then **adjust the token bucket's fill rate** to match reality:

- If the header says 4,200 remaining with 45 minutes until reset, the effective rate is 4200/45 = 93/min.
- If our internal bucket thought we had 3,000 remaining (because we're not the only consumer of this token), we recalibrate downward.

This is important because **we may not be the only client using this OAuth token.** The customer might have other integrations hitting the same API with the same credentials. Adaptive rate limiting keeps us honest by trusting the source's own accounting over our internal estimate.

Not all connectors support this — it's declared in the capability model (`headers` block). For connectors that don't expose remaining budget, we rely purely on our internal limiter state and treat the configured limit as a hard ceiling.

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
│  │  Limiter Pool (in-memory, Redis-backed for multi-node)          │   │
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

### The `Release()` Method — Why It Matters

For token-bucket limiters, there's nothing to release — a token is consumed and refilled by time. But for concurrency-based limiters (Jira, Salesforce), the slot must be explicitly freed when the request completes. This is why the interface has a `Release()` method in addition to `Allow()`.

The executor must call `Release()` in a `defer` to guarantee slot return even on panics:

```go
if err := limiter.CheckConnector(ctx, tenantID, userID, connectorID); err != nil {
    return err
}
defer limiter.Release(tenantID, userID, connectorID)

rows, meta, err := connector.Fetch(ctx, principal, sourceQuery)
```

For token-bucket connectors, `Release()` is a no-op. The limiter type handles this internally.

---

## State Sharing: Single-Node vs Multi-Node

### Single-Node (Prototype)

The prototype uses an in-process `sync.RWMutex`-guarded map. This is correct for a single gateway process — all requests see the same limiter state.

```go
type Service struct {
    mu      sync.RWMutex
    buckets map[string]*rate.Limiter
}
```

### Multi-Node (Production)

In production, the query gateway runs as multiple pods behind a load balancer. If each pod maintains its own independent limiter state, the effective limit is multiplied by the number of pods:

```
3 gateway pods, each with a token bucket of 83 req/sec for GitHub:
  → effective limit: 83 × 3 = 249 req/sec
  → actual GitHub limit: 83 req/sec
  → result: 3x overshoot → 429 from GitHub
```

The fix: **shared state via Redis.**

```
Pod A → Redis EVAL (atomic check + decrement) → allow/deny
Pod B → Redis EVAL (atomic check + decrement) → allow/deny
Pod C → Redis EVAL (atomic check + decrement) → allow/deny
```

All four levels (L1–L4) use Redis-backed shared state for consistency across pods. L1 and L2 may have a longer sync interval than L3/L4 since they're internal fairness controls, but they still use Redis because pod count is unknown at runtime. A slight overshoot on a tenant's global QPS doesn't cause external damage, but we still maintain a shared state to avoid the multiplier problem.

**Redis rate-limit implementation**: We use Redis' `EVAL` command with a Lua script for atomic token-bucket operations. This avoids race conditions between check-and-decrement across pods. For concurrency semaphores, Redis `SETNX` with TTL provides distributed slot management with automatic slot recovery if a pod crashes without releasing.

```
Key schema in Redis:
  rl:l3:{tenant_id}:{connector_id}        → token count + last_refill_at
  rl:l4:{tenant_id}:{user_id}:{conn_id}   → token count + last_refill_at
  rl:sem:{tenant_id}:{connector_id}        → set of active slot holders (with TTL)
```

### Failure Mode: Redis Down

If Redis is unreachable, the Rate-Limit Service falls back to local in-process limiters with a **conservative multiplier**:

```
Configured limit: 5000 req/hour
Number of gateway pods: 3 (known from k8s Endpoints API or env var)
Fallback limit per pod: 5000 / 3 = 1666 req/hour
```

This is intentionally pessimistic — it under-allocates to avoid overshooting the external limit. When Redis recovers, the service switches back to shared state seamlessly.

The fallback is logged and emitted as a Prometheus metric (`ratelimit_redis_fallback_active{connector=...}`) so that operators know the system is running degraded.

---

## Interaction with Cache Layer

Rate-limit checks and cache checks happen in a specific order, and the ordering matters:

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

**The critical question: should a cache hit consume a rate-limit token?**

**Answer: No.** The rate-limit token represents an external API call. A cache hit avoids the API call entirely. Consuming a token on cache hits would artificially reduce the budget available for actual API calls.

But wait — we checked L3/L4 *before* the cache lookup. If the cache hits, we consumed a token unnecessarily.

**Resolution**: The check at step 1-2 is a **reservation**, not a consumption. If the cache hits, the reservation is cancelled (the token is returned to the bucket). If the cache misses, the reservation is committed.

```go
reservation := limiter.Reserve(tenantID, connectorID)

if cached, _, ok := cache.Get(cacheKey, maxStaleness); ok {
    reservation.Cancel()   // cache hit — return the token
    return cached, nil
}

// cache miss — reservation stands, proceed to live fetch
rows, meta, err := connector.Fetch(ctx, principal, sourceQuery)
```

For concurrency semaphores, the slot is acquired before cache check (because we need it if the cache misses), and released immediately on cache hit. The slot hold duration on cache hits is negligible (microseconds).

---

## Async Overflow Path

When L3 or L4 denies a request, the default behavior is a 429 with `Retry-After`. But for some query patterns, this is a poor UX — the user submitted a legitimate query that just happened to arrive when the budget was exhausted.

The async overflow path provides an alternative:

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
  → job executor picks up the query when rate-limit budget refills
  → client polls /v1/jobs/{id} or receives webhook notification
```

**Which queries are eligible for async?** Not all. The decision is based on:

- **Tenant policy**: Admin enables/disables async per connector.
- **Query type**: JOIN queries that touch multiple connectors can be partially async (one connector is throttled, the other isn't — fetch the available one now, queue the throttled one).
- **Client hint**: The request can include `"allow_async": true` to opt in.

Queries that are NOT eligible for async (always synchronous 429):
- Queries with `max_staleness: 0` (the user explicitly wants live data — an async response that arrives in 45 seconds defeats the purpose).
- Queries from interactive UIs where the user is waiting for an immediate response (indicated by client hint or inferred from the presence of short timeout).

---

## Error Vocabulary

Every connector maps source-specific errors to a standard code before returning to the executor. The executor handles only these codes; raw source messages are preserved in the `message` field for observability but never surface to the user as-is.

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

Key properties of the error response:

- **`level`**: Tells the caller *which* rate limit was hit (`tenant_global`, `user_global`, `tenant_connector`, `user_connector`). Different levels require different remediation.
- **`retry_after_seconds`**: Computed from the limiter's refill schedule. For token buckets, it's time until enough tokens refill to serve the request. For concurrency semaphores, it's an estimate based on average request duration.
- **`budget`**: Shows the full budget picture so the caller can make informed decisions (e.g., "I've used 4,800 of 5,000 — I should stop polling and wait").
- **`suggestion`**: Human-readable guidance. This varies by level:
  - L1/L2: "Reduce query frequency or contact your tenant admin."
  - L3: "Retry after Xs, increase max_staleness, or upgrade API plan."
  - L4: "You've consumed your share of the connector budget. Other users are also querying this source."
- **`async_available`**: If the async overflow path is enabled for this connector and the query is eligible, the client can re-submit with `"allow_async": true`.

---

## Fairness Across Tenants — Head-of-Line Blocking Avoidance

In a multi-tenant deployment, all tenants share gateway pods. A tenant that's being rate-limited (waiting for `Retry-After` to elapse) must not block other tenants' queries.

The design avoids this by:

1. **Non-blocking rate-limit checks**: `Allow()` never sleeps. It returns immediately with allow or deny. The caller decides whether to wait, retry, or fail.
2. **Per-tenant limiter isolation**: Each tenant's limiter is a separate object. A slow `Reserve()` call for ACME doesn't lock the mutex for BigCorp's limiter.
3. **Errgroup per query, not per tenant**: The executor uses `errgroup` per query execution. A rate-limited connector in one query doesn't affect another query's goroutines.
4. **No shared queues**: The async overflow path uses per-tenant queue partitions (e.g., SQS message groups keyed by tenant_id). A tenant with 10,000 queued jobs doesn't delay another tenant's first job.

---

## Observability

The Rate-Limit Service emits the following signals:

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

On denial, the span includes `retry_after_seconds` and `budget.*` attributes, making it easy to correlate rate-limit events with query latency in Jaeger.

### Alerts

| Alert | Condition | Severity |
|---|---|---|
| `RateLimitBudgetLow` | Tenant's L3 budget < 10% remaining for any connector | Warning |
| `RateLimitBudgetExhausted` | Tenant's L3 budget = 0 and queries are being denied | Critical |
| `RateLimitRedisFallback` | Redis-backed limiter fell back to local state | Warning |
| `RateLimitAsyncQueueDepth` | Async overflow queue > 1,000 jobs for any tenant | Warning |
| `RateLimitSlotLeak` | Concurrency semaphore slots held > 5 minutes (probable leak) | Critical |

---

## Prototype vs Production Gap

| Aspect | Prototype (current) | Production design |
|---|---|---|
| **Key dimensions** | `tenant:connector` (2D) | `tenant`, `tenant:user`, `tenant:connector`, `tenant:user:connector` (4D) |
| **Limiter type** | Token bucket only | Token bucket + semaphore + composite, selected by connector capability |
| **Burst** | Always allowed | Only for connectors that declare `model: token_bucket` |
| **State sharing** | In-process (`sync.RWMutex`) | Redis-backed for all levels (L1–L4) across all pods |
| **User fairness** | Not enforced | L2 + L4 ensure no single user starves others |
| **Cache interaction** | Token consumed even on cache hit | Reservation pattern — cancel on cache hit |
| **Async overflow** | Not implemented | On L3/L4 denial, eligible queries routed to async job queue |
| **Adaptive limits** | Static config only | Reads SaaS response headers to recalibrate budget |
| **Release semantics** | No release (token bucket only) | `Release()` for concurrency semaphore connectors |
| **Redis fallback** | N/A (single node) | Local state with conservative per-pod allocation |
| **Observability** | Basic allowed/denied in response | Prometheus metrics, OTel spans, alerts |

---

## Summary

| Design Decision | Rationale |
|---|---|
| **Four-level hierarchy** | Each level protects against a different failure mode. Collapsing any level leaves a gap. |
| **Connector-declared rate-limit model** | Burst is a connector property, not a system property. A universal token bucket is incorrect for slot-based APIs. |
| **Policy in control plane, enforcement in data plane** | Configuration changes (new tenant override) don't require data plane redeployment. Enforcement is on the hot path — must be sub-millisecond. |
| **Redis-backed state for L1–L4** | All four levels use Redis-backed shared state to avoid the pod-count multiplier problem. L1/L2 may have a longer sync interval than L3/L4 since they're internal fairness controls, but they still require global consistency across pods. |
| **Reservation pattern with cache** | Cache hits shouldn't consume rate-limit tokens. The reservation is cancelled on hit, committed on miss. |
| **Async overflow as opt-in** | Not all queries benefit from async. Interactive queries should fail fast; batch queries should queue gracefully. |
| **Adaptive recalibration from headers** | We may not be the only consumer of the API credentials. The source's own accounting is more accurate than ours. |
