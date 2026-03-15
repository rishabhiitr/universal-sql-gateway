# Freshness and Caching Strategy

This is the caching and freshness strategy for the data plane. It covers *why* we cache, *what* we cache at each tier (source results, materializations, query results), the predicate pushdown model that makes the source cache effective, the freshness mechanics, join materialization with the three-tier cache flow, entitlement interaction, and the production cache topology.

---

## 1. Why Cache

Three forces make caching non-negotiable:

1. **Rate limits are scarce.** Every SaaS API imposes per-app or per-tenant rate limits. GitHub gives 5,000 requests/hr per token; Jira Cloud caps at ~100 req/s per tenant. Without caching, a busy enterprise would exhaust its API budget in minutes — blocking every user for the remainder of the window.

2. **SaaS APIs are slow.** Typical connector round-trip latencies range from 50ms (GitHub search, warm) to 800ms+ (Salesforce SOQL with large result sets). Our P50 SLO is 500ms end-to-end. That budget can't absorb a live API call on every query.

3. **Queries overlap heavily.** In an enterprise with 10,000 users, most queries hit the same underlying data with minor variations — different filters, different columns, different users looking at the same project. Caching lets one API call serve hundreds of queries.

The goal is simple: **minimize live SaaS API calls while bounding how stale the data can get.** Everything else in this doc serves that goal.

---

## 2. When to Cache

Every connector fetch is always cached. The question is never "should we cache?" — it's "how long is the cached data valid?"

Two inputs control that decision:

- **Connector-configured TTLs** (soft and hard, per connector) — the source's freshness contract
- **Per-query `max_staleness` hints** from the caller — the consumer's freshness tolerance

The only exception: a connector can be configured with `always_fresh: true`, which forces every request to bypass cache and fetch live from the source. This exists for sources where staleness is unacceptable (e.g., a payments API where amounts must be real-time). It's an admin-level connector config, not a per-query knob.

The default flow: **serve from cache if within TTL, revalidate asynchronously if the soft TTL has expired.** The caller never waits for revalidation unless the hard TTL has also expired. Details in [§4 — How We Cache](#4-how-we-cache).

---

## 3. What to Cache

### Why Source-Level Caching Is the Primary Tier (Not Query-Level)

The naive approach is to cache query results keyed by the full normalized SQL:

```
cache_key = hash(tenant_id + connector + normalized_sql)
```

As the *primary* cache, this breaks down at scale:

```sql
SELECT * FROM jira.issues WHERE assignee = 'alice'
SELECT * FROM jira.issues WHERE assignee = 'bob'
SELECT * FROM jira.issues WHERE assignee = 'charlie'
```

10,000 users → 10,000 cache entries, all querying the same underlying table with minor variations. Add filters like `status`, `priority`, `project`, date ranges — the number of unique queries is effectively infinite. Hit rate drops to single digits as the primary caching strategy.

The fundamental problem: **queries are infinite, but the data they fetch overlaps heavily.** This is why source-level caching (§3 below) is the primary tier. Query-result caching exists as a conditional third tier (§6) — only for expensive compute paths where the key-space cost is justified.

### The Solution: Cache at the Fetch Level (Pushed Predicates)

Instead of caching the final query result, we cache **what the connector actually fetched from the SaaS API.** The cache key is the pushed predicate set, not the full SQL.

When a query arrives, the planner decomposes it:

```
User SQL:   SELECT * FROM jira.issues
            WHERE assignee = 'rishabh' AND priority = 'high' AND status = 'open'

Planner decides:
  Push to Jira API:   assignee = 'rishabh'        ← Jira's API supports this filter
  Filter locally:     priority = 'high'            ← cheaper to filter post-fetch
                      status = 'open'              ← cheaper to filter post-fetch
```

The cache key becomes:

```
cache_key = hash(tenant + connector + table + pushed_predicates)
         = hash(acme + jira + issues + {assignee: 'rishabh'})
```

Now watch what happens with subsequent queries:

```
Query 1: WHERE assignee = 'rishabh' AND priority = 'high'
  pushed = {assignee: 'rishabh'}  →  CACHE MISS → fetch from Jira → cache it

Query 2: WHERE assignee = 'rishabh' AND status = 'open'
  pushed = {assignee: 'rishabh'}  →  CACHE HIT → filter status locally

Query 3: WHERE assignee = 'rishabh'
  pushed = {assignee: 'rishabh'}  →  CACHE HIT

Query 4: WHERE assignee = 'rishabh' AND priority = 'high' AND created > '2024-01-01'
  pushed = {assignee: 'rishabh'}  →  CACHE HIT → filter priority + date locally
```

One API call. Four queries served. That's the difference between a 5% hit rate and a 70-80% hit rate.

### Predicate Classification: What Gets Pushed vs. Stripped

Every predicate in a WHERE clause falls into exactly one of three categories. The classification is static — driven by the connector's declared schema, not by runtime guesses about result size.

**Category 1 — Partition predicates.** Always pushed. Always in the cache key.

These are the columns the upstream API *requires* to scope a request. You cannot call the API without them, and data across different partitions is unrelated — caching across them is meaningless.

| Connector | Partition Dimensions | Reason |
|---|---|---|
| GitHub | `repo`, `owner` | API requires a repo — no "list all PRs across all repos" endpoint |
| Jira | `project` | API scopes to a project key |
| Salesforce | `object_type` | Each object (Account, Contact) is a separate endpoint |

**Category 2 — Entitlement predicates.** Never pushed. Never in the cache key.

These come from the user's token and tenant policy, not from SQL. RLS predicates (`owner_id = 'alice'`), CLS masks (`salary → MASKED`), implicit scopes added by the Entitlement Service. Keeping them out of the cache key is what makes the cache tenant-scoped rather than user-scoped. See [§5 — How Entitlements Interact with Cache](#5-how-entitlements-rlscls-interact-with-cache).

**Category 3 — Value filters.** Stripped by default; pushed only when they bound an unbounded fetch.

Everything else: `state = 'open'`, `priority = 'high'`, `assignee = 'alice'`, `created_at > '2024-01-01'`. The rules:

- **Low-cardinality columns** (e.g. `state`: 3 possible values) — strip. One broader cache entry serves all values; post-filter locally in microseconds.
- **High-cardinality columns** (e.g. `assignee`: 5,000 users) — strip. Pushing creates per-user cache entries, defeating shared caching.
- **Time boundaries** (e.g. `created_at > '2024-01-01'`) — push a broadened version (round down to month boundary) to prevent unbounded historical fetches while preserving reuse.

Default bias: strip and post-filter. Local filtering is microseconds on a few thousand rows; cache reuse across queries is worth far more.

**How connectors declare this**

Each connector's capability model includes a schema declaration:

```yaml
github_connector:
  tables:
    pull_requests:
      partition_columns: [repo, owner]       # always pushed
      filter_columns: [state, author, label] # stripped by default
      api_filterable: [state, author]        # CAN be pushed if planner decides to
```

`api_filterable` tells the planner which columns the API *can* filter on — but the planner is not obligated to push them. It will only push an `api_filterable` column if the response size without it would be unmanageable (e.g., a repo with 50K PRs where pushing `state=open` reduces it to 200).

### Predicate Subsumption

Cache lookup is not exact-match only. During cache orchestration (step 4 of the [executor's per-source loop](executor.md#3-per-source-execution-loop)), the executor checks whether an existing cached fetch is a **superset** of the current query's pushed predicates:

```
Cached fetch:  {assignee: 'rishabh'}              → 3000 rows (all projects)
New query:     {assignee: 'rishabh', project: 'ENG'}  → subset

Executor detects: cached predicate is BROADER than needed
  → Filter project = 'ENG' locally from cached data
  → CACHE HIT, no API call
```

The planner classifies predicates (§3 above). The executor uses that classification at runtime to perform cache lookup, subsumption checks, and post-fetch filtering.

---

## 4. How We Cache

### Serve First, Cache Asynchronously

The primary path: serve the query result to the caller, then update the cache in a background goroutine. The caller never blocks on a cache write.

For a cache miss:
1. Fetch from the SaaS API (caller waits for this)
2. Return result to caller immediately
3. Write to cache in background (fire-and-forget goroutine)

For a soft-TTL-expired cache hit:
1. Serve the stale cached data to the caller immediately (zero added latency)
2. Fire one background revalidation goroutine
3. Next request benefits from refreshed cache

### Soft TTL + Hard TTL + ETag Revalidation

A single fixed TTL is too coarse. The right model is a **two-tier TTL with conditional revalidation** — the same pattern HTTP caches use with `stale-while-revalidate`.

#### Cache Entry Structure

```go
type CacheEntry struct {
    Rows        []Row
    ETag        string        // from SaaS API response header, if supported
    SoftTTL     time.Time     // serve from cache; trigger background revalidation after this
    HardTTL     time.Time     // must re-fetch synchronously after this
}
```

#### TTL Defaults by Connector

| Connector | Soft TTL | Hard TTL | ETag support |
|---|---|---|---|
| GitHub | 5 min | 30 min | Yes (strong ETags; 304s don't cost rate-limit budget) |
| Jira | 2-3 min | 15 min | Partial (`If-Modified-Since` only) |
| Salesforce | 10 min | 10 min | No (soft = hard; no cheap revalidation) |
| Notion | 15 min | 15 min | No |
| Slack | 1 min | 5 min | No (high-velocity data) |

Both TTLs are overridable per-connector in config and per-query via the `max_staleness` hint.

#### Revalidation Flow

Revalidation is request-triggered. When a request arrives, the executor checks cache age:

```
Within soft TTL       → serve from cache. Done.
Between soft and hard → serve stale data to caller immediately.
                        If connector supports ETag/If-Modified-Since:
                          fire async conditional fetch (goroutine, caller doesn't wait)
                          304 → reset soft TTL, keep rows
                          200 → replace rows, reset both TTLs
                        If not:
                          data stays until hard TTL expires.
Past hard TTL         → synchronous re-fetch. Caller waits.
max_staleness = 0     → always re-fetch; update cache async so subsequent queries benefit.
```

A 304 costs almost nothing against the rate-limit budget — GitHub doesn't even count it. Connectors without ETag support have soft TTL = hard TTL (no cheap way to extend freshness); the Connector SDK's capability model declares this.

### The `max_staleness` Knob

Every query can carry a freshness hint:

```json
POST /v1/query
{
  "sql": "SELECT * FROM jira.issues WHERE assignee = 'rishabh'",
  "max_staleness": "300s"
}
```

This tells the system: "I'm okay with data up to 5 minutes old." The decision logic:

```
cache_entry = fetch_cache.lookup(pushed_predicates)

if cache_entry exists:
    age = now - cache_entry.fetched_at

    if age <= max_staleness:
        → CACHE HIT, filter locally, return
    elif connector supports ETag:
        → CONDITIONAL FETCH (If-Modified-Since)
        → 304 Not Modified? → serve cached, cheap API call
        → 200? → update cache, return fresh data
    else:
        → LIVE FETCH
else:
    → LIVE FETCH
```

Different query classes have different freshness needs:
- **Dashboard widget** (`max_staleness: 300s`): "show me open PRs" — 5-minute staleness is fine
- **Financial lookup** (`max_staleness: 0s`): "show me this invoice" — must be live
- **Report aggregate** (`max_staleness: 3600s`): "count of issues per project" — an hour is fine

#### Clamping: `max_staleness` Is a Hint, Not a Command

A client can always request `max_staleness: 0s`. If honored blindly, every request becomes a live fetch, burning the rate-limit budget for everyone.

The system enforces a **floor staleness** per connector:

```yaml
connectors:
  jira:
    min_cache_ttl: 30s       # floor — no query bypasses this
    default_cache_ttl: 300s   # used when max_staleness not specified
    hard_max_ttl: 3600s       # ceiling — data older than this always re-fetched
```

The effective staleness is:

```
effective_staleness = max(requested_max_staleness, connector.min_cache_ttl)
```

If clamped, the response includes a warning:

```json
{
  "meta": {
    "warning": "STALENESS_CLAMPED",
    "requested_staleness": "0s",
    "effective_staleness": "30s"
  }
}
```

#### Per-Tenant Live-Fetch Budget

Even with the floor, a tenant could send high volumes of unique queries (different pushed predicates) and exhaust rate limits. Each tenant gets a **live-fetch budget** per connector per time window:

```
tenant:acme:jira:live_fetch_budget → token bucket, e.g. 50 live fetches/min
```

When exhausted:
- Serve whatever cached data exists, even if stale
- Return `freshness_source: "CACHE_FORCED"` with actual `freshness_ms` so the caller knows
- Don't pretend it's fresh — be transparent

This isolates a noisy tenant from degrading the service for everyone else.

### Thundering Herd Prevention: singleflight

If 500 concurrent requests all observe an expired soft TTL for the same key, only **one** background revalidation goroutine should fire. Go's `singleflight` deduplicates in-flight revalidations per cache key:

```go
var revalidationGroup singleflight.Group

// Only one goroutine fires per key; the other 499 serve stale and return immediately
go revalidationGroup.Do(cacheKey, func() (interface{}, error) {
    return revalidate(cacheKey, entry.ETag)
})
```

Without this, 500 goroutines would each fire a conditional GET — burning 500 rate-limit tokens for what should be one API call.

### Cache Entry Structure

```
FetchCacheEntry {
    key:                hash(tenant + connector + table + pushed_predicates)
    pushed_predicates:  {assignee: "rishabh"}
    rows:               [...3000 rows...]
    fetched_at:         2026-02-27T10:00:00Z
    etag:               "W/abc123"           // if connector provided one
    ttl:                300s                  // from connector config
    row_count:          3000
}
```

### What the Response Tells the Caller

Every response includes freshness metadata so the caller always knows what they got:

```json
{
  "rows": [...],
  "columns": [...],
  "meta": {
    "freshness_ms": 142000,
    "freshness_source": "CACHE",
    "trace_id": "4bf92f3577b34da6",
    "rate_limit_status": {
      "connector": "jira",
      "remaining": 4820,
      "reset_at": "2026-02-27T10:15:00Z"
    }
  }
}
```

`freshness_source` values:
- **`LIVE_FETCH`**: Data was fetched fresh from the SaaS API right now.
- **`CACHE`**: Served from cache, within the requested `max_staleness`.
- **`CONDITIONAL_FETCH`**: ETag check confirmed cache is still valid. Cheap API call.
- **`CACHE_FORCED`**: Staler than requested, but served because live-fetch budget was exhausted. Caller should check `freshness_ms` to see actual age.

---

## 5. Join Materialization and Three-Tier Cache

Both sides of a cross-app join go through the fetch-cache pipeline independently. Once both results are available (from cache or live), the join executes locally:

```
Query: SELECT p.title, i.status
       FROM github.prs p JOIN jira.issues i ON p.jira_key = i.issue_key
       WHERE p.state = 'open'

Step 1: Fetch github.prs (pushed: state='open')     → cache or live
Step 2: Fetch jira.issues (pushed: none or broad)    → cache or live
Step 3: Hash join in-process (DuckDB / in-memory)    → local, fast
Step 4: Return joined result
```

If both sides are cache-warm, the entire join query completes with zero API calls.

### Three Tiers

**Tier 1 — Source cache** (always on). Caches raw connector responses keyed by `(tenant + connector + table + pushed_predicates)`. Results < ~5 MB live in Redis; larger results spill to encrypted S3 Parquet with a Redis pointer (`s3://cache/<tenant>/source/<key>.parquet`). This is what avoids redundant API calls and rate-limit burn.

**Tier 2 — Large result materialization** (conditional). When join output or a single connector fetch exceeds ~5 MB, the result is persisted as encrypted Parquet on S3. Short TTL (≤ 30 min), crypto-shredded on tenant offboarding. S3 instead of node-local storage because stateless routing means no guarantee a query lands on the same node twice. An S3 GET (~20-50 ms) is still far cheaper than re-fetching from two SaaS APIs and re-joining.

**Tier 3 — Query result cache** (conditional). Final computed results — post-RLS/CLS, post-join — are cached when the compute path was expensive (S3 download → DuckDB → join) or the result set is non-trivial (>1K rows). Key is `(tenant, SQL hash, user entitlements)`. Small results go to Redis; large results go to S3 Parquet. User entitlements are part of the key because two users with different RLS policies get different results.

Why not cache every query result? The key-space includes the SQL hash *and* user entitlements — caching cheap queries (e.g., a 50-row single-source lookup already warm in Redis) would bloat the key-space for near-zero latency savings. Conditional caching + short TTLs + LRU eviction keep the key-space bounded. The payoff is on repeated expensive queries: dashboards, reports, and concurrent users running the same SQL get instant results with zero recomputation.

**Why materialization caching matters.** It pays off in two cases: (1) the join itself is expensive — re-fetching from two SaaS APIs and re-computing a hash join over 100K+ rows costs minutes of wall time and rate-limit budget, while an S3 GET is ~30 ms; (2) source caches have expired but the joined result is still within the caller's staleness tolerance — the materialized result has its own TTL, so it can outlive the individual source caches and avoid unnecessary re-fetches.

### Lookup

The executor checks all three tiers. Query result cache is checked first (cheapest hit); source cache and materialization are checked in parallel:

| Query Result Cache | Source Cache | Materialization | Action |
|---|---|---|---|
| Hit | — | — | Return immediately. Zero compute. |
| Miss | All sides hit | — | Re-join locally from source cache. |
| Miss | — | Hit | Pull Parquet from S3. Avoids SaaS API calls. |
| Miss | Partial hit | Miss | Fetch missing sides, join with cached sides. |
| Miss | Miss | Miss | Full live fetch. |

---

## 6. How Entitlements (RLS/CLS) Interact with Cache

The entitlement service enforces row-level security (RLS) and column-level security (CLS). At first glance, this seems to conflict with the cache strategy:

```
Cache strategy:    Push FEWER predicates → broader fetch → higher cache reuse
Entitlements:      Apply MORE filters (RLS) → restrict what user sees → security
```

### The Resolution: RLS/CLS Apply After Cache, Not Before Fetch

The critical insight: **RLS/CLS filters are NOT pushed to the connector API.** They're applied **locally on the cached data**, post-fetch.

```
Query: SELECT * FROM salesforce.accounts WHERE region = 'APAC'
User: Alice (only owns APAC accounts, can't see salary column)

Step 1 — Planner builds pushed predicates (for cache/fetch):
  Pushed to API: {}  or  {account_type: 'customer'}    ← broad for cache reuse
  Cache key: hash(tenant + salesforce + accounts + pushed_predicates)

Step 2 — Fetch or cache hit:
  Returns ALL accounts for this tenant (unfiltered at user level)

Step 3 — Apply user's WHERE clause locally:
  Filter: region = 'APAC'

Step 4 — Apply RLS locally (from Entitlement Service):
  Filter: owner_id = 'alice'  (automatic, invisible to user)

Step 5 — Apply CLS locally (from Entitlement Service):
  Mask: salary → '***MASKED***'

Step 6 — Return to user
```

### Why This Is Correct

If RLS predicates were pushed to the API:

```
User Alice:  pushed = {owner_id: 'alice'}  → fetch Alice's rows → cache
User Bob:    pushed = {owner_id: 'bob'}    → fetch Bob's rows → separate cache entry
User Carol:  pushed = {owner_id: 'carol'}  → another fetch
```

This is per-user caching. 10,000 users → 10,000 cache entries. Hit rate collapses.

Instead:

```
Tenant-level fetch:  pushed = {}              → fetch ALL rows → cached ONCE
User Alice query:    apply RLS locally         → filter to Alice's rows
User Bob query:      apply RLS locally         → filter to Bob's rows (same cache)
User Carol query:    apply RLS locally         → same cache entry
```

One API call serves all users. RLS is just an in-memory filter.

### The Complete Cache Flow

A cross-app JOIN query showing how the three tiers, entitlements, and compute interact end-to-end:

```
┌──────────────────────────────────────────────────────────────────┐
│  User Query                                                      │
│  SELECT p.title, i.status                                        │
│  FROM github.prs p JOIN jira.issues i ON p.key = i.key           │
│  WHERE p.state = 'open'                                          │
└──────────────┬───────────────────────────────────────────────────┘
               │
               ▼
┌──────────────────────────────────────────────────────────────────┐
│  Tier 3 — Query Result Cache                                     │
│  Key = hash(tenant + SQL + user entitlements)                    │
│                                                                  │
│  HIT  → return immediately, zero compute                         │
│  MISS ↓                                                          │
└──────────────┬───────────────────────────────────────────────────┘
               │
               ▼
┌──────────────────────────────────────────────────────────────────┐
│  Tier 2 — Materialization Cache (S3 Parquet)                     │
│  Key = hash(tenant + join signature + source versions)           │
│                                                                  │
│  HIT  → pull Parquet from S3 (~30ms), skip all API calls         │
│  MISS ↓                                                          │
└──────────────┬───────────────────────────────────────────────────┘
               │
               ▼
┌──────────────────────────────────────────────────────────────────┐
│  Tier 1 — Source Cache (per connector, in parallel)              │
│                                                                  │
│  ┌─────────────────────────────┐ ┌─────────────────────────────┐ │
│  │ github.prs                  │ │ jira.issues                 │ │
│  │ key = tenant+github+prs+   │ │ key = tenant+jira+issues+{} │ │
│  │       {state:'open'}       │ │                             │ │
│  │                             │ │                             │ │
│  │ < 5MB → Redis     (HIT)    │ │ > 5MB → S3 Parquet  (HIT)  │ │
│  │ > 5MB → S3 Parquet         │ │                             │ │
│  │ MISS  → live SaaS API call │ │ MISS  → live SaaS API call │ │
│  └─────────────────────────────┘ └─────────────────────────────┘ │
└──────────────┬───────────────────────────────────────────────────┘
               │
               ▼
┌──────────────────────────────────────────────────────────────────┐
│  Compute (local)                                                 │
│  1. Hash join in-process (DuckDB / in-memory)                    │
│  2. Apply user WHERE filters                                     │
│  3. Apply RLS (row filtering)        ← entitlements here         │
│  4. Apply CLS (column masking)       ← entitlements here         │
│  5. ORDER BY / LIMIT                                             │
│                                                                  │
│  If result expensive to recompute → write back to Tier 3 / Tier 2│
└──────────────────────────────────────────────────────────────────┘
```

The flow is top-down: cheapest check first (Tier 3, a single Redis GET), then materialization (Tier 2, an S3 GET), then source caches (Tier 1, per-connector). A hit at any tier short-circuits everything below it. Cache optimizes the fetch (tenant-level, broad); entitlements secure the response (user-level, narrow).

---

## 7. Cache Eviction & Data Retention

We are not a data store. Cached data is transient and bounded:

- **TTL eviction**: Every entry expires per connector config (30s to 1hr typically).
- **LRU eviction**: When cache memory exceeds the configured limit, least-recently-used entries are evicted first.
- **Tenant offboarding**: All cache entries for a tenant are purged immediately. Since cache is keyed by tenant, this is a simple prefix delete.
- **S3 lifecycle**: Large source results, materialized joins, and large query results spill to S3. Cleaned up by S3 object lifecycle rules (bulk expiry) + a cron job for minute-level TTL precision (S3 lifecycle has ~1-day minimum granularity). All S3 data is tenant-key-encrypted.
- **Cold start**: When Redis restarts, cache is cold — queries hit live API until the cache warms. S3 data survives restarts but is TTL-bound regardless.

This keeps us in "query accelerator" territory. All cached data is transient (TTL-bound, LRU-evicted, or lifecycle-expired). Tenant offboarding triggers crypto-shredding — rotating the tenant's KMS key makes all S3 and Redis data unrecoverable without waiting for TTL expiry.

---

## 8. Cache Topology (Redis Cluster)

Production uses Redis Cluster (6+ nodes, 16384 hash slots). The prototype uses an in-process Go map.

```
Cache key = SHA256(tenant + connector + table + pushed_predicates)
  → hashes to slot (0-16383) → one Redis node → one GET, ~0.5ms
```

Keys scatter naturally via consistent hashing — no connector-affine pinning (that creates hot nodes). Different tenants and predicate combinations spread across slots even if one connector dominates traffic.

Lookups are always single-key. JOINs issue two pipelined GETs (~1ms total). Hot read keys aren't a problem — Redis handles millions of reads/sec per node. Large entries (>1MB) store a pointer to S3, not the data itself.

---

## Summary

| Aspect | Decision |
|---|---|
| **Why cache** | Rate limits are scarce, SaaS APIs are slow, queries overlap heavily. |
| **Source of truth** | Always the SaaS API. Cache never overrides it. |
| **Cache granularity** | Pushed predicates (what we asked the API), not full SQL. |
| **Hit rate strategy** | Push fewer predicates, filter locally. Predicate subsumption for broader matches. |
| **Freshness control** | `max_staleness` hint, clamped by connector floor. ETag for cheap revalidation. |
| **Abuse protection** | Floor staleness + per-tenant live-fetch budget. Transparent `CACHE_FORCED` fallback. |
| **Join strategy** | Each side cached independently. Join computed locally from cached fetches. |
| **Query result cache** | Conditional — only expensive paths (S3→DuckDB→join) or non-trivial results (>1K rows). Keyed by `(tenant, SQL hash, user entitlements)`. |
| **Storage tiering** | Redis for hot/small data (< ~5 MB). S3 Parquet for large source results, materializations, and large query results. |
| **Data retention** | All cached data is transient. TTL + LRU eviction in Redis. S3 lifecycle rules + cron for minute-level cleanup. Crypto-shredding on tenant offboarding. |
| **Entitlements** | RLS/CLS applied post-fetch on cached data. Cache is tenant-scoped, not user-scoped. |
| **Transparency** | Every response carries `freshness_ms`, `freshness_source`, and `rate_limit_status`. |
| **Topology** | Redis Cluster with consistent hashing. Single-key lookups. S3 spill for large entries. |
