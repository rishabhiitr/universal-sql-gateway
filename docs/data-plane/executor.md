# Executor

The executor is the runtime orchestrator for query execution. It is the single component that owns all runtime concerns: entitlement enforcement, cache orchestration, rate-limit reservation, connector fanout, post-fetch security filtering, join execution, memory-aware routing, and spill behavior.

This document covers *what* the executor does (and what it deliberately delegates), *how* entitlements are enforced (the most security-critical path), the per-source execution loop, join mechanics, spill strategy, and the sync/async routing decision.

---

## 1. What the Executor Does (and What It Doesn't)

The executor is the **conductor** — it calls other components but contains minimal domain logic itself.

**What it owns:**
- Entitlement pre-check (table-level access via OPA)
- Concurrent connector fanout (`errgroup`)
- Cache orchestration (lookup, reservation, write-back)
- Rate-limit reservation (L3/L4 per-connector layers)
- Post-fetch RLS/CLS application
- Join execution (hash join / DuckDB spill)
- Projection, ordering, limit
- Memory monitoring and spill-to-disk trigger
- Sync-to-async handoff

**What it receives from the planner:**

The planner is a pure function — SQL in, `QueryPlan` out, no I/O. The executor never looks at the raw SQL field. It only consumes the structured plan:

```go
type QueryPlan struct {
    SQL          string         // original SQL (logging only, never re-parsed)
    Projections  []ColumnRef    // SELECT columns
    Sources      []SourceQuery  // one per connector, with pushdown filters
    Join         *JoinSpec      // nil for single-source queries
    PostFilters  []FilterExpr   // applied after fetch + join
    OrderBy      []OrderBySpec
    Limit        int
    MaxStaleness time.Duration
}

type SourceQuery struct {
    ConnectorID string
    Table       string
    Alias       string
    Filters     []FilterExpr   // pushed down to this connector
    Limit       int
}
```

This separation means planners can be swapped (cost-based, ML-assisted) without touching the executor.

**What it receives from connectors:**

Connectors return `[]Row` and `SourceMeta`. The executor treats connectors as opaque — it never reaches into connector internals, never knows JQL or SOQL or GitHub API params. It only knows `SourceQuery` and the rows that come back.

---

## 2. Entitlement Enforcement

Entitlements are the first thing the executor checks and the last thing it applies. The design follows a **two-phase model**: a fast binary access check before any work happens, then fine-grained row/column filtering after data is fetched.

This is the most security-critical path in the data plane. Get it wrong and users see data they shouldn't. Get it right and one cached API call safely serves thousands of users with different permission levels.

### 2.1 Pre-Flight: OPA Table-Level Access Check

Before any connector is called, before any rate-limit budget is consumed, the executor asks one question per table: **can this user access this table at all?**

This is a binary allow/deny decision. It runs via the OPA sidecar — a separate container in the same pod that holds the full policy bundle in memory:

```
CheckTableAccess(ctx, userToken, "github.pull_requests")
  │
  └─► POST http://localhost:8181/v1/data/authz/allow
        input: {
          "user":     {"id": "alice", "tenant_id": "acme", "roles": ["developer"]},
          "resource": {"table": "github.pull_requests", "connector": "github"}
        }
        → {"result": true}   // deny → ENTITLEMENT_DENIED, query aborted
```

The call is loopback-local (<1ms). No network hop, no serialization overhead.

**What OPA decides:**
- Can user with role `developer` access table `github.pull_requests`? → yes/no
- Can user with role `viewer` access table `salesforce.accounts`? → yes/no

**What OPA does NOT decide:**
- Which rows the user can see (that's RLS — executor concern, post-fetch)
- Which columns are masked (that's CLS — executor concern, post-fetch)
- Whether the user has OAuth scopes for the connector (that's scope validation — also executor)

This boundary is intentional. OPA is excellent at binary policy decisions evaluated against a structured input document. Row-level and column-level filtering require iterating over result sets — that's application logic, not policy logic.

For a JOIN query touching two tables, the executor makes two OPA calls (one per table). Both must return `allow`. If either denies, the entire query is rejected with `ENTITLEMENT_DENIED` — no partial execution.

### 2.2 OPAL: How Policies Reach OPA

OPA by itself is a stateless policy engine — it evaluates policies against input, but it doesn't know where policies come from or when they change. **OPAL (Open Policy Administration Layer)** is the delivery mechanism that keeps OPA's policy bundle current.

#### The Push Model

```
┌───────────────────────────────────────────────────────────────┐
│  CONTROL PLANE                                                 │
│                                                                │
│  ┌──────────────┐    ┌──────────────┐    ┌──────────────────┐ │
│  │ Policy Store  │    │ Tenant       │    │ Connector        │ │
│  │ (Rego files)  │───►│ Registry     │───►│ Schema Catalog   │ │
│  └──────────────┘    └──────────────┘    └──────────────────┘ │
│          │                    │                    │            │
│          └────────────────────┴────────────────────┘            │
│                               │                                │
│                    ┌──────────▼──────────┐                     │
│                    │  OPAL Server        │                     │
│                    │  • Watches policy   │                     │
│                    │    store for changes│                     │
│                    │  • Compiles policy  │                     │
│                    │    bundles per      │                     │
│                    │    tenant/connector │                     │
│                    │  • Pushes to OPAL   │                     │
│                    │    clients via      │                     │
│                    │    WebSocket        │                     │
│                    └──────────┬──────────┘                     │
└───────────────────────────────┼────────────────────────────────┘
                                │
                    WebSocket push (~1-2s propagation)
                                │
┌───────────────────────────────▼────────────────────────────────┐
│  DATA PLANE (per gateway pod)                                   │
│                                                                │
│  ┌──────────────────┐      ┌──────────────────┐               │
│  │  OPAL Client     │─────►│  OPA Sidecar     │               │
│  │  (sidecar)       │      │  (localhost:8181) │               │
│  │  • Receives push │      │  • Holds full     │               │
│  │  • Updates OPA's │      │    policy bundle  │               │
│  │    policy bundle │      │    in memory      │               │
│  │  • Updates OPA's │      │  • Evaluates      │               │
│  │    data store    │      │    allow/deny     │               │
│  └──────────────────┘      └────────┬─────────┘               │
│                                      │                         │
│                              loopback (<1ms)                   │
│                                      │                         │
│                            ┌─────────▼─────────┐              │
│                            │  Query Executor    │              │
│                            │  CheckTableAccess()│              │
│                            └───────────────────┘              │
└────────────────────────────────────────────────────────────────┘
```

#### What OPAL Pushes

The policy bundle that OPAL delivers to each OPA sidecar contains two things:

1. **Rego policy rules** — the logic that evaluates allow/deny:

```rego
package authz

default allow = false

allow {
    # User's role must be in the table's allowed_roles
    role := input.user.roles[_]
    role == data.table_policies[input.resource.table].allowed_roles[_]
}

allow {
    # Admins can access everything
    input.user.roles[_] == "admin"
}
```

2. **Data documents** — the tenant-specific configuration that the rules evaluate against:

```json
{
  "table_policies": {
    "github.pull_requests": {
      "allowed_roles": ["admin", "developer"]
    },
    "jira.issues": {
      "allowed_roles": ["admin", "developer", "viewer"]
    },
    "salesforce.accounts": {
      "allowed_roles": ["admin", "sales"]
    }
  }
}
```

The Rego rules rarely change (they define the authorization model). The data documents change frequently (new tenants, new connectors, role changes). OPAL optimizes for this — it can push a data-only update without recompiling the full policy bundle.

#### Why OPAL, Not Static Bundles

OPA supports a pull-based bundle model out of the box (poll an HTTP endpoint every N seconds). OPAL adds:

| Capability | Static Bundles | OPAL |
|---|---|---|
| **Propagation latency** | Polling interval (30s-5min typical) | WebSocket push (~1-2s) |
| **Revocation speed** | Minutes (next poll cycle) | Seconds (immediate push on policy change) |
| **Tenant-specific policies** | Must compile all tenants into one bundle | Can push per-tenant data updates independently |
| **Data source integration** | Manual — you build the bundle pipeline | Built-in — OPAL watches Postgres, Git, APIs |
| **Selective updates** | Full bundle re-download | Incremental data patches |

The revocation speed matters most. When an admin revokes a user's access to a table, the policy change must propagate in seconds, not minutes. A 5-minute polling interval means a revoked user can query for 5 more minutes. OPAL's WebSocket push closes this gap to ~1-2 seconds.

#### Failure Mode: OPA Unavailable

If the OPA sidecar is unreachable (crashed, restarting), the executor **denies all requests**. This is fail-closed by design:

```go
func (e *Executor) checkTableAccess(ctx context.Context, principal *Principal, table string) error {
    result, err := e.opaClient.Query(ctx, "data.authz.allow", input)
    if err != nil {
        // OPA unreachable — fail closed, not open
        return qerrors.New(qerrors.CodeEntitlementDenied,
            "authorization service unavailable — access denied",
            "", 5, nil)  // Retry-After: 5s
    }
    if !result.Allowed {
        return qerrors.New(qerrors.CodeEntitlementDenied,
            fmt.Sprintf("access denied to table %s", table),
            "", 0, nil)
    }
    return nil
}
```

The OPA sidecar has a liveness probe. If it crashes, k8s restarts it within seconds. The OPAL client reconnects and receives the latest policy bundle. During the gap, queries fail with a retryable error — this is preferable to silently allowing unauthorized access.

### 2.3 Post-Fetch: RLS then CLS

After connector fetch (or cache hit), before join, the executor applies row-level security (RLS) and column-level security (CLS). These are applied **on cached data, not at fetch time** — the cache is tenant-scoped, not user-scoped. One API call serves all users of a tenant; each user's security view is derived locally.

This is the same model as Postgres row-level security: the table stores all rows, the policy filters at query time.

#### Execution Order

RLS runs before CLS. No point masking columns on rows that are about to be dropped:

```
Cached rows (tenant-scoped, broad)
  │
  ▼
Apply RLS: filter rows where row[column] != principal[field]
  │  (e.g., 10,000 rows → 2,000 rows for this user)
  │
  ▼
Apply CLS: replace restricted column values with mask string
  │  (e.g., email → "[REDACTED]" for non-admin roles)
  │
  ▼
Rows ready for join / projection / return
```

#### The Policy DSL

Entitlement rules are expressed as static YAML, version-controlled alongside connector config:

```yaml
tables:
  jira.issues:
    allowed_roles: [admin, developer, viewer]
    row_filters:
      - column: assignee
        principal_field: username    # row.assignee must equal caller's username
        except_roles: [admin]        # admins see all rows
    column_masks:
      email:
        except_roles: [admin]
        mask: "[REDACTED]"

  github.pull_requests:
    allowed_roles: [admin, developer]
    row_filters: []                  # no row-level restrictions
    column_masks: []                 # no column masking

  salesforce.accounts:
    allowed_roles: [admin, sales]
    row_filters:
      - column: owner_id
        principal_field: user_id
        except_roles: [admin, sales_manager]
    column_masks:
      revenue:
        except_roles: [admin, finance]
        mask: "***"
```

This is simple, auditable, and version-controlled. In production it evolves toward a dynamic store (OPA + OPAL) that supports tenant-specific overrides, time-bound rules, and programmatic policy generation. The static DSL remains the default for tenants that don't need dynamic policies.

#### RLS/CLS Application Code

```go
func ApplyRLS(rows []Row, filters []RowFilter, principal *Principal) []Row {
    result := make([]Row, 0, len(rows))
    for _, row := range rows {
        keep := true
        for _, f := range filters {
            if principalExempt(principal, f.ExceptRoles) {
                continue
            }
            if fmt.Sprint(row[f.Column]) != principal.Field(f.PrincipalField) {
                keep = false
                break
            }
        }
        if keep {
            result = append(result, row)
        }
    }
    return result
}

func ApplyCLS(rows []Row, masks []ColumnMask, principal *Principal) []Row {
    for i, row := range rows {
        for _, m := range masks {
            if !principalExempt(principal, m.ExceptRoles) {
                row[m.Column] = m.Mask
            }
        }
        rows[i] = row
    }
    return rows
}
```

#### Production Extension: RLS Predicate Injection

For large tables where post-fetch RLS filtering would discard 90%+ of cached rows, the planner can inject RLS predicates directly into `SourceQuery.Filters` before pushdown:

```
Normal path (default):
  Fetch ALL tenant rows → cache → apply RLS locally (filter 80%)

Optimized path (large tables):
  Inject RLS predicate into SourceQuery → push to API → cache per-user key
  Trade-off: lose cache reuse, gain narrower fetch
```

This is a production optimization, not the default. The planner makes this call based on estimated row count and RLS selectivity. When triggered, the cache key includes the RLS predicate, making the entry user-scoped for that specific table.

### 2.4 The Principal: Roles vs Scopes

The `Principal` struct carries two distinct auth concepts:

```go
type Principal struct {
    UserID   string
    TenantID string
    Roles    []string  // coarse-grained: admin, developer, viewer
    Scopes   []string  // fine-grained OAuth: github:read, jira:issues:read
}
```

**Roles** (coarse-grained) drive the policy DSL — `allowed_roles`, `row_filters`, `column_masks`. These come from the OIDC `id_token` claims issued by the enterprise IdP (Okta, Azure AD, Google Workspace).

**Scopes** (fine-grained) drive connector-level access — a user with role `developer` but missing scope `jira:issues:read` is denied at the connector level, not the policy level. These come from the OAuth access token issued by the source SaaS app.

This mirrors real enterprise SSO: the IdP issues identity (roles), the SaaS app issues delegated access (scopes). Both are enforced; neither is sufficient alone.

**Where each is checked:**

| Check | What's evaluated | Where | When |
|---|---|---|---|
| Table access (OPA) | Roles | Pre-flight | Before any connector call |
| Connector scope | Scopes | Per-source execution loop | Before connector fetch |
| RLS row filtering | Roles + Principal fields | Post-fetch | After cache hit or live fetch |
| CLS column masking | Roles | Post-fetch | After RLS |

---

## 3. Per-Source Execution Loop

For each `SourceQuery` in the plan, the executor runs the following steps. Multiple sources run concurrently via `errgroup` with structured cancellation — if any source fails fatally, siblings are cancelled via shared context.

```
1. Entitlement pre-check (CheckTableAccess via OPA)
   │   deny → ENTITLEMENT_DENIED, no further work
   │
2. Scope validation (does principal have required OAuth scopes for this connector?)
   │   missing scope → ENTITLEMENT_DENIED
   │
3. Rate-limit reservation (L3: tenant×connector + L4: user×connector)
   │   exhausted + allow_async → enqueue to async path
   │   exhausted + !allow_async → 429 Retry-After
   │
4. Cache lookup (keyed by pushed predicates — see freshness-and-caching.md §3)
   ├── HIT + within max_staleness → cancel rate-limit reservation, return cached rows
   ├── HIT + soft TTL expired → serve stale, fire background revalidation
   └── MISS or hard TTL expired → proceed to step 5
   │
5. Connector.Fetch(ctx, principal, sourceQuery)
   │   connector translates SourceQuery to SaaS API call (JQL, REST, SOQL)
   │   rows returned through bulkhead (timeout, circuit breaker, panic recovery)
   │
6. Cache write (background goroutine — caller doesn't wait)
   │
7. Apply RLS (filter rows based on principal identity)
   │
8. Apply CLS (mask restricted columns)
   │
9. Return rows to executor's merge phase
```

Steps 7-8 apply regardless of whether the data came from cache or live fetch. The cache holds tenant-scoped data; the user's security view is always computed at query time.

---

## 4. Join Execution

### Join Type

The current SQL subset supports `INNER JOIN` with equality predicates (`ON a.key = b.key`).

### Default Strategy: Hash Join

For bounded result sets, the executor uses hash join:

1. Build a hash map from the smaller side.
2. Probe with the larger side and emit merged rows.

Total complexity is `O(n + m)` time and `O(min(n, m))` space.

```go
func hashJoin(leftRows, rightRows []Row, leftKey, rightKey string) []Row {
    // Always build on the smaller side
    if len(leftRows) > len(rightRows) {
        leftRows, rightRows = rightRows, leftRows
        leftKey, rightKey = rightKey, leftKey
    }

    bucket := make(map[string][]Row, len(leftRows))
    for _, row := range leftRows {
        key := fmt.Sprint(row[leftKey])
        bucket[key] = append(bucket[key], row)
    }

    out := make([]Row, 0, len(rightRows))
    for _, right := range rightRows {
        key := fmt.Sprint(right[rightKey])
        for _, left := range bucket[key] {
            merged := make(Row, len(left)+len(right))
            for k, v := range left {
                merged[k] = v
            }
            for k, v := range right {
                merged[k] = v
            }
            out = append(out, merged)
        }
    }
    return out
}
```

Note: RLS/CLS are applied **before** the join, not after. Each side is filtered to the user's security view before rows enter the hash map. This means the join operates on already-authorized data — no risk of joining on rows the user shouldn't see.

---

## 5. Spill-to-Disk Strategy

When in-memory execution would exceed budget, the executor routes to spill mode.
Use two layers intentionally: **node-local Parquet scratch files** for the active execution, and **S3-backed cache/materialization** for large artifacts that must survive beyond the current process or be reused by later queries (as described in `freshness-and-caching.md`).

### Spill Triggers

| Condition | Metric | Action |
|---|---|---|
| Single connector returns >50K rows | Row count in Fetch response | Write to local Parquet scratch for this execution; publish to S3-backed source cache/materialization if the cache tier decides to retain it |
| Hash join build side >100K rows | Row count in hash table | Build hash table on DuckDB (disk-backed) instead of in-memory map |
| Process memory >70% of GOMEMLIMIT | `runtime.MemStats.HeapInuse` | Trigger forced spill for active queries; switch to disk-backed execution |
| Query result set >10MB | Estimated bytes of projected rows | Persist result to S3 and return a handle/presigned URL instead of inline rows |

### Spill Flow

```
Normal path:
  Connector.Fetch() -> []Row -> hash join in-memory -> project -> return inline

Spill path:
  Connector.Fetch() -> []Row -> exceeds threshold
    -> Write rows to local Parquet scratch file
    -> Execute join in DuckDB on Parquet files
    -> Write result to Parquet
    -> If reusable/TTL-bound, persist encrypted Parquet to S3 via cache/materialization layer
    -> If small, read back to memory and return inline
    -> If large, return handle/presigned URL backed by S3
```

### DuckDB Runtime Modes

- **Sync or smaller spill cases**: in-process DuckDB (CGo) for low latency.
- **Async or spill-heavy cases**: DuckDB sidecar (same pod, shared disk) for stronger memory isolation.

Decision rule:
- `<50K rows/side` and interactive latency target: keep in-process.
- `>=50K rows/side`, spill expected, or async requested: route to sidecar.
- Very large async outputs: write result to S3 and return a job/result handle.

### Why Executor Owns Spill

Spill is an execution concern, not a connector concern:
- Connectors fetch source rows and return `[]Row`.
- The executor decides join strategy and memory-safe routing.
- Local scratch files (`/tmp/*.parquet`) are executor-owned execution detail; durable reusable artifacts are handed off to the cache/materialization layer for S3 persistence.

---

## 6. Sync vs Async Paths

Not all queries fit synchronous HTTP windows (10-30s). Slow paths include:
- Large pagination windows from source APIs
- Rate-limit wait windows
- Slow upstream APIs
- Large joins
- Spill and S3 write latency

### Routing Decision

```
Query arrives → parse → plan

Is result estimatable?
  Yes, <10K rows per source, all source caches warm?
    → SYNC path (inline response)

  Yes, >50K rows or join of >10K × >10K?
    → ASYNC path (durable job queue, e.g. SQS)

  Unknown (first query, no cache, no stats)?
    → Start SYNC, switch to ASYNC if timeout approaching

Rate limit status?
  All connectors have budget → proceed on chosen path
  Any connector budget exhausted, async overflow enabled → ASYNC
  Any connector budget exhausted, no async overflow → 429 Retry-After
```

### Sync-to-Async Handoff

The most interesting case: a query starts synchronous but the executor realizes it won't finish in time.

```
Gateway sets 10s timeout context
  → Executor starts fetching concurrently
  → After 7s, one connector is still paginating
  → 3s remaining, estimated 5s more needed
  → If allow_async in request → persist async job state, enqueue to durable queue (e.g. SQS), return 202 with job_id
  → If !allow_async → wait until timeout → SOURCE_TIMEOUT with partial results hint
```

The executor monitors `ctx.Deadline()` in the fetch loop. It doesn't commit to sync or async upfront — starts sync, escalates if needed. On handoff, it persists a resumable job spec/checkpoint (query plan, principal, freshness settings, and any completed-source state), enqueues that job to a durable queue, and returns a `job_id` for client polling. It does not try to suspend and serialize live in-flight goroutines.

---

## 7. Key Design Decisions

| Decision | Rationale |
|---|---|
| **Executor as the conductor** | Single component owns all orchestration (cache, rate-limit, entitlements, join, spill). Simpler than distributed coordination between executor and connectors. |
| **OPA for table-level AuthZ only** | Keeps the authorization boundary clean. OPA decides binary allow/deny. Row filtering and column masking are executor concerns applied post-fetch on cached data. |
| **OPAL push, not OPA pull** | WebSocket push propagates policy changes in ~1-2s. Polling-based bundles take 30s-5min. Revocation speed matters — a revoked user shouldn't query for minutes. |
| **Fail-closed on OPA unavailability** | If the authorization service is down, deny all requests. Preferable to silently allowing unauthorized access. Retryable error with short Retry-After. |
| **RLS/CLS post-fetch on cached data** | Cache is tenant-scoped for high reuse. RLS/CLS are in-memory filters applied per-user at query time. Same model as Postgres row-level security. |
| **Roles + Scopes, both enforced** | Roles (from IdP) control policy rules. Scopes (from SaaS OAuth) control connector access. Neither alone is sufficient — mirrors real enterprise SSO. |
| **Hash join by default, hybrid DuckDB spill path** | Hash join is O(n+m) and optimal for equi-joins under 50K rows per side. Use in-process DuckDB for latency-sensitive small spill cases; use sidecar DuckDB for async/large spill cases to isolate memory and protect sync traffic. |
| **Planner as pure function** | No I/O, no side effects. Produces a QueryPlan struct that the executor consumes. Enables planner replacement (cost-based, ML-assisted) without touching the executor. |
| **Memory accounting is approximate** | Go doesn't support per-goroutine memory limits. Approximate accounting (row count × estimated row size) is sufficient to prevent catastrophic OOM. Exact tracking would require instrumenting every allocation — too costly. |
| **Async path is opt-in** | Not all queries benefit from async. Interactive queries should fail fast (429 with Retry-After). Batch/reporting queries should queue gracefully via a durable job queue (for example, SQS) and return `202 Accepted` with a `job_id`. Client declares intent via `allow_async`. |
| **Connector panic recovery** | A single `recover()` in the bulkhead prevents one connector's crash from taking down the entire gateway process. This is defense-in-depth alongside circuit breakers and timeouts. |
