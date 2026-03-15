# Data Plane

This is the entry point to the data plane documentation. It covers the component map, query planner internals, sync/async execution paths, and a full end-to-end trace. For deep dives into individual components, see:

- [Executor](executor.md) — entitlements, per-source execution loop, join mechanics, spill strategy, sync/async routing
- [Freshness & Caching](freshness-and-caching.md) — predicate classification, TTLs, revalidation, materialization cache
- [Rate-Limit Service](rate-limit-service.md) — four-layer model, token buckets, connector rate-limit adapters, async overflow


---


## Component Map


```
┌─────────────────────────────────────────────────────────────────────────────┐
│                              DATA PLANE                                     │
│                                                                             │
│  ┌───────────────────────────────────────────────────────────────────────┐  │
│  │  QUERY GATEWAY (HTTP Ingress)                                         │  │
│  │  • AuthN (JWT/OIDC validation)                                        │  │
│  │  • L1/L2 rate-limit check (tenant-global, user-global)                │  │
│  │  • Request validation (max SQL length, page size bounds)              │  │
│  │  • Timeout context injection                                          │  │
│  │  • Trace ID generation                                                │  │
│  └───────────────────────────────┬───────────────────────────────────────┘  │
│                                  │                                          │
│  ┌───────────────────────────────▼───────────────────────────────────────┐  │
│  │  QUERY PLANNER                                                        │  │
│  │  • SQL → AST (vitess-sqlparser / ANTLR4)                              │  │
│  │  • Capability discovery (which connectors support which predicates)   │  │
│  │  • Filter classification (pushdown vs post-fetch)                     │  │
│  │  • Join plan generation (hash join vs materialization)                │  │
│  │  • Cost/freshness hints                                               │  │
│  │  • Emit QueryPlan struct                                              │  │
│  └───────────────────────────────┬───────────────────────────────────────┘  │
│                                  │                                          │
│  ┌───────────────────────────────▼───────────────────────────────────────┐  │
│  │  QUERY EXECUTOR                                                       │  │
│  │  • Entitlement pre-check (CheckTableAccess)                           │  │
│  │  • Concurrent connector fanout (errgroup)                             │  │
│  │  • Cache orchestration (reservation pattern)                          │  │
│  │  • L3/L4 rate-limit enforcement (per-connector)                       │  │
│  │  • Post-fetch RLS/CLS application                                     │  │
│  │  • Join execution (hash join / sort-merge)                            │  │
│  │  • Projection, ordering, limit                                        │  │
│  │  • Memory monitoring & spill-to-disk trigger                          │  │
│  └───────────────────────────────┬───────────────────────────────────────┘  │
│                                  │                                          │
│         ┌────────────────────────┼─────────────────────────┐               │
│         │                        │                         │               │
│  ┌──────▼──────────┐   ┌────────▼──────────┐   ┌──────────▼─────────┐     │
│  │  CONNECTOR POOL │   │  CONNECTOR POOL   │   │  CONNECTOR POOL    │     │
│  │  (GitHub)       │   │  (Jira)           │   │  (Salesforce, ...) │     │
│  │  • Goroutine    │   │  • Goroutine      │   │  • Goroutine       │     │
│  │    pool         │   │    pool           │   │    pool            │     │
│  │  • Auth/token   │   │  • Auth/token     │   │  • Auth/token      │     │
│  │    refresh      │   │    refresh        │   │    refresh         │     │
│  │  • Pagination   │   │  • Pagination     │   │  • Pagination      │     │
│  │  • Error map    │   │  • Error map      │   │  • Error map       │     │
│  │  • Retry logic  │   │  • Retry logic    │   │  • Retry logic     │     │
│  └────────┬────────┘   └────────┬──────────┘   └──────────┬─────────┘     │
│           │                     │                          │               │
│  ┌────────▼─────────────────────▼──────────────────────────▼──────────┐    │
│  │  SHARED SERVICES                                                   │    │
│  │  • Source Cache (Redis / in-process)                               │    │
│  │  • Rate-Limit Service (token buckets, semaphores)                  │    │
│  │  • Entitlement Engine (RLS/CLS)                                    │    │
│  │  • Encryption SDK (envelope encryption; per-tenant DEK/KEK)        │    │
│  │  • Materialization Store (S3/DuckDB for spill + large joins)       │    │
│  └────────────────────────────────────────────────────────────────────┘    │
└─────────────────────────────────────────────────────────────────────────────┘
```

**Key principle — the executor is the conductor.** The planner is a pure function (SQL in, `QueryPlan` out, no I/O). The executor owns all runtime orchestration: when to call connectors, how to join results, when to switch from in-memory to spill, when to escalate to async.

The gateway enforces L1/L2 rate limits and AuthN before any planning happens. L3/L4 are enforced per-source in the executor. See [rate-limit-service.md](rate-limit-service.md) for the full four-layer model. Table-level authorization (OPA sidecar) and post-fetch RLS/CLS are covered in [executor.md](executor.md#2-entitlement-enforcement).


---


## Planner Internals


### Parsing Pipeline


```
User SQL string
    │
    ▼
┌─────────────────────────────────┐
│  SQL Parser (vitess-sqlparser)  │
│  → Produces raw AST             │
└──────────────┬──────────────────┘
              │
              ▼
┌─────────────────────────────────┐
│  AST Walker                     │
│  → Extract projections          │
│  → Extract source tables        │
│  → Extract WHERE predicates     │
│  → Extract JOIN spec            │
│  → Extract ORDER BY / LIMIT     │
└──────────────┬──────────────────┘
              │
              ▼
┌─────────────────────────────────┐
│  Filter Classifier              │
│  → Resolve aliases to sources   │
│  → Classify each predicate:     │
│     pushdown → SourceQuery      │
│     post-fetch → PostFilters    │
│  → Consult capability model     │
└──────────────┬──────────────────┘
              │
              ▼
┌─────────────────────────────────┐
│  Plan Enrichment                │
│  → Attach freshness hints       │
│  → Set per-source limits        │
│  → (Future) Cost estimation     │
└──────────────┬──────────────────┘
              │
              ▼
         QueryPlan struct
```

SQL parsing is CPU-bound but fast (~50–100μs per query with vitess-sqlparser). At 1k QPS that's ~10% of one core — not a bottleneck.

Filter classification (which predicates get pushed to connectors vs. applied locally post-fetch) is covered in [freshness-and-caching.md §3](freshness-and-caching.md#3-what-to-cache).


### QueryPlan Struct

The `QueryPlan` is the interface boundary between planner and executor. The executor never looks at the raw SQL field — it only consumes the structured fields. This means planners can be swapped (cost-based, ML-assisted) without touching the executor.

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


### Connector Query Translation

The connector never receives raw SQL. It receives a `SourceQuery` and translates it into whatever the SaaS API speaks:

```
SourceQuery {ConnectorID: "jira", Filters: [{assignee=alice}, {status=open}]}
 │
 └─► Jira connector → JQL: assignee = alice AND status = open
 └─► GitHub connector → REST: ?state=open&per_page=100
 └─► Salesforce connector → SOQL: SELECT Id FROM Case WHERE OwnerId = 'alice'
```

| Component | Knows | Doesn't know |
|---|---|---|
| **Planner** | SQL syntax, filter classification | JQL, SOQL, GitHub API params |
| **Executor** | `SourceQuery`, cache keys, rate limits | JQL, SOQL, GitHub API params |
| **Connector** | SaaS API language, pagination, auth | SQL, other connectors, join logic |


---


## Sync vs Async Execution Paths

The executor supports both inline (sync) and job-queue (async) execution. The routing decision and sync-to-async handoff are covered in [executor.md §6](executor.md#6-sync-vs-async-paths). Below are the wire-level flows.

### Sync Path

```
Client                     Gateway                       Executor
 │                          │                              │
 │  POST /v1/query          │                              │
 │ ────────────────────►    │                              │
 │                          │  Execute(ctx, plan, req)     │
 │                          │ ───────────────────────────► │
 │                          │                              │── fetch connectors
 │                          │                              │── join + filter
 │                          │                              │── RLS/CLS
 │                          │  ◄── QueryResponse           │
 │  ◄── 200 OK + rows      │                              │
```

Response:
```json
{
 "columns": [...],
 "rows": [...],
 "freshness_ms": 142,
 "freshness_source": "CACHE",
 "rate_limit_status": [...],
 "trace_id": "abc123"
}
```

### Async Path

```
Client                     Gateway                Job Queue               Async Worker
 │  POST /v1/query          │                       │                       │
 │  allow_async: true       │                       │                       │
 │ ────────────────────►    │                       │                       │
 │                          │── budget exhausted    │                       │
 │                          │  enqueue(query)       │                       │
 │                          │ ─────────────────────►│                       │
 │  ◄── 202 Accepted       │                       │                       │
 │      job_id: "xyz789"   │                       │                       │
 │      poll_url: /v1/jobs/xyz789                   │                       │
 │                          │                       │── budget refills      │
 │                          │                       │  dequeue(query)       │
 │                          │                       │ ─────────────────────►│
 │                          │                       │                       │── fetch + join
 │                          │                       │                       │── write to S3
 │  GET /v1/jobs/xyz789     │                       │                       │
 │ ────────────────────►    │                       │                       │
 │  ◄── 200 COMPLETED      │                       │                       │
 │      result_url: s3://...│                       │                       │
```

202 response:
```json
{
 "job_id": "xyz789",
 "status": "QUEUED",
 "estimated_completion_ms": 45000,
 "poll_url": "/v1/jobs/xyz789"
}
```


---


## End-to-End Trace

A single JOIN query traced from HTTP ingress to response.

### The Query

```sql
SELECT gh.title, gh.state, j.issue_key, j.status, j.assignee
FROM github.pull_requests gh
JOIN jira.issues j ON gh.jira_issue_id = j.issue_key
WHERE gh.state = 'open'
LIMIT 10
```

User: Alice (tenant: acme-corp, role: developer). Request carries `max_staleness: 300s`. Connector credentials (GitHub API key, Jira OAuth service account) are stored in Vault, separate from Alice's JWT.

---

### Step 1 — Gateway (~2ms)

1. Extract/generate `trace_id = "4bf92f3577b34da6"`
2. Validate JWT → `Principal{user_id: "alice", tenant_id: "acme-corp", roles: ["developer"]}`
3. **L1** rate-limit check (tenant-global): 4820/5000 remaining → ✓
4. **L2** rate-limit check (user-global): 180/200 remaining → ✓
5. Inject timeout context: `ctx = context.WithTimeout(parent, 30s)`
6. Validate request: SQL length 267 chars ✓, LIMIT 10 ✓
7. Forward to planner

---

### Step 2 — Planner (~8ms)

1. Parse SQL → AST
2. Extract table references: `["github.pull_requests", "jira.issues"]`
3. Capability lookup from schema catalog:
- GitHub v1.3.0: pushdown `state`, `author`, `repo`; ETag yes; token bucket 5k/hr
- Jira v2.1.0: pushdown `assignee`, `status`, `project`; ETag yes; concurrency 10 + 300/min
4. Classify predicates:
- `gh.state = 'open'` → GitHub supports `?state=open` and it's high-selectivity (50K PRs → 200) → **PUSH**
- No predicates on Jira side → `pushed_predicates: {}`
5. Build source queries:
  ```
  GitHub: table=pull_requests, pushed={state: "open"},  cache_key=hash("acme-corp:github:pull_requests:{state:open}")
  Jira:   table=issues,        pushed={},               cache_key=hash("acme-corp:jira:issues:{}")
  ```
6. Join strategy: join key `gh.jira_issue_id = j.issue_key`, estimated GitHub ~200 rows / Jira ~10K rows → **hash join**, build on GitHub side
7. Emit `QueryPlan`

**Cumulative: 10ms**

---

### Step 3 — Executor Pre-Flight (~3ms)

1. OPA table-access checks (loopback):
- `github.pull_requests` → `{"result": true}` ✓
- `jira.issues` → `{"result": true}` ✓
2. Fetch RLS/CLS policies:
- GitHub: no row filter, no column masks
- Jira: `row_filter = (assignee = 'alice' OR reporter = 'alice')`, no column masks

**Cumulative: 13ms**

---

### Step 4 — Concurrent Connector Fanout (~5ms)

Two goroutines via `errgroup`, running in parallel:

**Goroutine 1 — GitHub**:
- L3 check (tenant×connector): 4200/5000 → ✓
- L4 check (user×connector): 180/300 → ✓
- Cache lookup: age = 142s ≤ max_staleness 300s → **CACHE HIT**, 200 rows returned, tokens cancelled

**Goroutine 2 — Jira**:
- L3/L4 checks: ✓
- Cache lookup: age = 280s ≤ max_staleness 300s → **CACHE HIT**, 10,000 rows returned

Zero external API calls. Zero rate-limit tokens consumed.

**Cumulative: 18ms**

---

### Step 5 — Post-Fetch RLS (~12ms)

- GitHub (no RLS): 200 rows → 200 rows
- Jira (RLS: `assignee = 'alice' OR reporter = 'alice'`): 10,000 rows → 2,000 rows (80% filtered)

**Cumulative: 30ms**

---

### Step 6 — Hash Join (~7ms)

Build phase (GitHub, 200 rows): `hashMap[jira_issue_id] → []Row`, ~150 unique keys, ~1MB

Probe phase (Jira, 2,000 rows): O(1) lookup per row → ~800 matches, ~2MB joined result

**Cumulative: 37ms**

---

### Step 7 — Projection & LIMIT (<1ms)

Keep `[gh.title, gh.state, j.issue_key, j.status, j.assignee]`, take first 10 rows.

**Cumulative: 38ms**

---

### Step 8 — Response (~2ms)

```json
{
 "rows": [
   {"gh.title": "Add user auth", "gh.state": "open", "j.issue_key": "PROJ-123", "j.status": "In Progress", "j.assignee": "alice"},
   ...
 ],
 "columns": [
   {"name": "gh.title", "type": "TEXT"},
   {"name": "gh.state", "type": "TEXT"},
   {"name": "j.issue_key", "type": "TEXT"},
   {"name": "j.status", "type": "TEXT"},
   {"name": "j.assignee", "type": "TEXT"}
 ],
 "meta": {
   "freshness_ms": 280000,
   "freshness_source": "CACHE",
   "trace_id": "4bf92f3577b34da6",
   "rate_limit_status": {
     "github": {"remaining": 4200, "reset_at": "2026-02-27T10:15:00Z"},
     "jira":   {"remaining": 290,  "reset_at": "2026-02-27T10:01:00Z"}
   }
 }
}
```

**Total query latency: 40ms**
