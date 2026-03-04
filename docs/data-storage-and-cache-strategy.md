# Data Storage & Cache Strategy

## Core Principle: We Are a Federated Query Engine, Not a Data Store

This is the most important design decision in the entire caching layer: **the SaaS API is always the source of truth.** Every query can and will hit the live API if needed. The cache is a transparent accelerator — it reduces API calls, protects rate-limit budgets, and improves latency for repeated query patterns. It never affects result correctness.

If the cache has data and it's fresh enough — great, fast path. If not — live fetch, still correct, just slower. There is no "coverage gap" problem because we never promise to have all the data locally. The source always does.

This is exactly how production federated engines work (Trino, Presto, Athena Federated). They don't store data. They query live sources, and cache is optional and opportunistic.

---

## Why Not Cache at the Query Level?

The naive approach is to cache query results keyed by the full normalized SQL:

```
cache_key = hash(tenant_id + connector + normalized_sql)
```

This breaks down immediately at scale:

```sql
SELECT * FROM jira.issues WHERE assignee = 'alice'
SELECT * FROM jira.issues WHERE assignee = 'bob'
SELECT * FROM jira.issues WHERE assignee = 'charlie'
```

10,000 users → 10,000 cache entries, all querying the same underlying table with minor variations. Add filters like `status`, `priority`, `project`, date ranges — the number of unique queries is effectively infinite. Cache hit rate drops to single digits. Useless.

The fundamental problem: **queries are infinite, but the data they fetch overlaps heavily.**

---

## The Solution: Cache at the Fetch Level (Pushed Predicates)

Instead of caching the final query result, we cache **what the connector actually fetched from the SaaS API.** The cache key is the pushed predicate set, not the full SQL.

### How It Works

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

### The Trade-off the Planner Makes

```
Push MORE predicates → smaller API response → less bandwidth → LOW cache reuse
Push FEWER predicates → larger API response → more bandwidth → HIGH cache reuse
```

This is a cost-based decision. The planner weighs:

- **Response size**: Broader fetch returns 1,000 rows? Manageable. 1M rows? Push more predicates to narrow it down.
- **Existing cache**: Is there already a cached broad fetch that covers this query? If yes, don't fetch at all.
- **Local filtering cost**: Cheap. DuckDB / in-memory filtering handles thousands of rows in microseconds.

### Predicate Subsumption

The planner can go further. Even if the pushed predicates don't match exactly, it checks whether an existing cached fetch is a **superset**:

```
Cached fetch:  {assignee: 'rishabh'}              → 3000 rows (all projects)
New query:     {assignee: 'rishabh', project: 'ENG'}  → subset

Planner detects: cached predicate is BROADER than needed
  → Filter project = 'ENG' locally from cached data
  → CACHE HIT, no API call
```

This is what makes the cache truly effective — it's not just exact-match lookup, it's set-containment reasoning on predicates.

### Predicate Classification: What Gets Pushed vs. Stripped

The planner doesn't make ad-hoc decisions about which predicates to push. Each predicate in the WHERE clause is classified into one of three categories, and the classification is driven jointly by the **connector's declared schema** and the **predicate's nature**.

**Category 1 — Partition predicates (always pushed, always in cache key)**

These define the natural scope of a connector fetch. Every connector declares which columns are *partition dimensions* — they correspond to how the SaaS API organizes data:

| Connector | Partition Dimensions | Why |
|---|---|---|
| GitHub | `repo`, `owner` | GitHub API scopes to a repo — you can't list PRs without specifying one |
| Jira | `project` | Jira API scopes to a project |
| Salesforce | `object_type` | Each Salesforce object (Account, Contact) is a separate API endpoint |

Partition predicates are always pushed because:
1. The API requires them (you can't fetch "all repos" in one call)
2. They define genuinely separate data domains — cache reuse across partitions is meaningless
3. They bound the fetch size to a manageable scope

**Category 2 — User-identity / entitlement predicates (never pushed, always applied post-fetch)**

These are derived from the user's token, role, or tenant policy — not from the SQL WHERE clause:

- RLS predicates: `owner_id = 'alice'`, `team IN ('engineering', 'platform')`
- CLS masks: `salary → MASKED`, `ssn → REDACTED`
- Implicit scoping: anything the Entitlement Service adds based on user context

These are **never** pushed to the API and **never** appear in the cache key. This is what makes the cache tenant-scoped rather than user-scoped. See [§ How Entitlements Interact with Cache](#how-entitlements-rlscls-interact-with-cache) below.

**Category 3 — Value filters (judgment call — push or strip based on cardinality)**

These are the remaining WHERE predicates from the user's SQL: `state = 'open'`, `priority = 'high'`, `assignee = 'alice'`, `created_at > '2024-01-01'`.

The planner decides whether to push or strip each one based on a simple heuristic:

```
IF the filter column has LOW cardinality (state: open/closed/merged → 3 values)
   → STRIP it. Don't push.
   → One broader cache entry serves queries for ALL state values.
   → Post-filter locally (microseconds on a few thousand rows).

IF the filter column has HIGH cardinality (assignee among 5000 users)
   → STRIP it. This is effectively a user-identity filter.
   → Pushing it creates per-user cache entries — defeats the purpose.

IF the filter defines a TIME BOUNDARY (created_at > '2024-01-01')
   → Push a BROADENED version (e.g., round down to month boundary).
   → Prevents unbounded historical fetches while preserving cache reuse.
```

In practice, most Category 3 predicates are stripped. The default bias is toward broader fetches because local filtering is cheap and cache reuse is valuable.

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

**The "select all" problem doesn't exist**

Because partition predicates are always pushed, you never fetch "all data in the tenant." The broadest possible fetch is "all rows within one partition" (e.g., all issues in one Jira project, all PRs in one GitHub repo). This is the connector's natural scope — the SaaS API is designed to return data at this granularity.

---

## The `max_staleness` Knob

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

### Clamping: `max_staleness` Is a Hint, Not a Command

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

### Per-Tenant Live-Fetch Budget

Even with the floor, a tenant could send high volumes of unique queries (different pushed predicates) and exhaust rate limits. Each tenant gets a **live-fetch budget** per connector per time window:

```
tenant:acme:jira:live_fetch_budget → token bucket, e.g. 50 live fetches/min
```

When exhausted:
- Serve whatever cached data exists, even if stale
- Return `freshness_source: "CACHE_FORCED"` with actual `freshness_ms` so the caller knows
- Don't pretend it's fresh — be transparent

This isolates a noisy tenant from degrading the service for everyone else.

---

## Cache Entry Structure

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

---

## ETag / Conditional Fetch

When the cache entry is stale but not expired past `hard_max_ttl`, we try a conditional fetch before doing a full re-fetch:

```
System → Jira API:
  GET /rest/api/3/search?jql=assignee=rishabh
  Headers: If-Modified-Since: 2026-02-27T10:00:00Z

Jira responds:
  304 Not Modified  → serve cached data, update fetched_at, near-zero cost
  200 OK            → new data, replace cache entry
```

This is powerful because a 304 response costs almost nothing against the rate-limit budget but keeps the cached data validated. Not all connectors support this — the Connector SDK's capability model declares whether ETag/conditional requests are available, and the planner uses this to decide the fetch strategy.

---

## What the Response Tells the Caller

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

## Cache Eviction & Data Retention

We are not a data store. Cached data is transient and bounded:

- **TTL eviction**: Every entry expires per connector config (30s to 1hr typically).
- **LRU eviction**: When cache memory exceeds the configured limit, least-recently-used entries are evicted first.
- **Tenant offboarding**: All cache entries for a tenant are purged immediately. Since cache is keyed by tenant, this is a simple prefix delete.
- **No persistent storage**: Cache lives in-memory (Redis for distributed, in-process for single-node). Nothing is written to disk or S3. When the process restarts, cache is cold — queries just hit live API until the cache warms up.

This keeps us squarely in "query accelerator" territory and avoids any compliance concerns around data retention, GDPR right-to-erasure, or SaaS vendor ToS restrictions on data mirroring.

---

## Join Materialization

For cross-app joins (e.g., GitHub PRs joined with Jira Issues), both sides go through the same fetch-cache pipeline independently. Once both fetch results are available (from cache or live), the join executes locally:

```
Query: SELECT p.title, i.status
       FROM github.prs p JOIN jira.issues i ON p.jira_key = i.issue_key
       WHERE p.state = 'open'

Step 1: Fetch github.prs (pushed: state='open')     → cache or live
Step 2: Fetch jira.issues (pushed: none or broad)    → cache or live
Step 3: Hash join in-process (DuckDB / in-memory)    → local, fast
Step 4: Return joined result
```

The materialized join result itself is **not cached** — it's computed on the fly from the two cached fetches. This avoids a combinatorial explosion of join-result cache entries while still benefiting from fetch-level caching on each side.

If both sides are cache-warm, the entire join query is answered with zero API calls.

### Two-Tier Architecture: Source Cache + Materialization Cache

In production, the caching layer extends to two tiers that serve different purposes:

**Tier 1 — Source Cache** (always on)
- Caches raw connector responses (what we fetched from the SaaS API)
- Keyed by `(connector + table + pushed_predicates + tenant)`
- In-memory (Redis for distributed, in-process for single-node)
- TTL driven by freshness config + `max_staleness` per query
- Avoids redundant API calls across different queries hitting the same source data

**Tier 2 — Materialization Cache** (conditional, production only)
- Caches joined/aggregated results
- Keyed by `(query plan hash + tenant)`
- Storage: encrypted Parquet on S3 or DuckDB per tenant
- Triggered when result sets exceed a memory threshold or the same join pattern repeats within a window
- Short TTL (≤ 30 min), crypto-shredded on tenant offboarding

### Lookup Strategy: Parallel Check, Prioritized Pull

These are **independent lookups, not a fallback chain.** Source cache answers "do I have raw data from each connector?" while materialization cache answers "do I have the joined result for this plan?" These are different questions, so we check both in parallel.

**Step 1**: Check existence of keys in both tiers (parallel, sub-ms for source cache, slightly longer for materialization)

**Step 2**: Decide which path to take based on what's available:

| Source Cache | Materialization Cache | Action |
|---|---|---|
| All sides hit | — | Pull from source cache (memory), re-join locally. Fastest path for small-medium result sets. |
| Miss | Hit | Pull from materialization (disk/S3). Avoids re-fetching from SaaS API entirely. |
| Partial hit | Miss | Fetch only missing sides from connectors, join with cached sides. |
| Miss | Miss | Full live fetch from all connectors. |

### Why Both Will Rarely Have Data Simultaneously

In practice, source cache and materialization cache having the same data at the same time is unlikely:

- If source cache is populated and fresh, queries are served from there — the join runs in-memory and there's no reason to write a materialization entry for small result sets.
- Materialization becomes valuable precisely when source cache TTLs have expired but the materialized join result is still within staleness bounds. This is its primary purpose: **avoiding re-fetch from the SaaS API when source cache has expired but the joined result is still fresh enough.**

The only scenario where both exist is a narrow race window right after a materialization write when source caches haven't expired yet. Not worth designing around — just prefer source cache (faster, in-memory) and move on.

### When Materialization Pays Off

Materialization is not about speed — source cache is in-memory (sub-ms) while materialization is disk/S3 (tens of ms). Materialization pays off when:

1. **The join itself is expensive**: Large result sets where re-joining costs more than an S3 read (e.g., joining 100K+ rows from each side).
2. **Source cache has expired but join result is still valid**: The materialized join can have a longer TTL than individual source caches, because the join work has already been done.
3. **Multiple queries hit the same join pattern**: Different users running the same cross-app query within a window share the materialized result.

### Prototype vs Production

For the prototype, only Tier 1 (source cache) exists. All joins are in-memory hash joins, which is correct for mock data with ~200 rows per side. Materialization (Tier 2) is a production concern documented here for architectural completeness.

---

## How Entitlements (RLS/CLS) Interact with Cache

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

### The Security Model

> "But the cache holds data the user isn't allowed to see!"

Yes, and this is safe because:

1. **Cache is internal** — users never access it directly. It's an in-memory store in the data plane. The API response only contains post-RLS/CLS data.
2. **This is how databases work** — Postgres stores all rows on disk. RLS policies filter at query time, not storage time. Same pattern here.
3. **The alternative is worse** — per-user fetches burn rate limits and kill cache hit rates. And the SaaS API already authorized the fetch using the tenant's service account — the data entered "your system" the moment the connector called the API.

### The Complete Pipeline

```
┌──────────────────────────────────────────┐
│  User Query (with user's context)        │
│  SELECT * FROM salesforce.accounts       │
│  WHERE region = 'APAC'                   │
└──────────────┬───────────────────────────┘
               │
               ▼
┌──────────────────────────────────────────┐
│  Query Planner                           │
│  1. Determine pushed predicates (broad)  │  ← for cache/fetch optimization
│  2. Determine local filters (narrow)     │  ← user's WHERE clause
│  3. Query Entitlement Service:           │
│     - Get RLS filters for this user      │  ← automatic row filtering
│     - Get CLS masks for this user        │  ← column masking/removal
└──────────────┬───────────────────────────┘
               │
               ▼
┌──────────────────────────────────────────┐
│  Fetch Layer (tenant-scoped)             │
│  Cache key = tenant + connector +        │
│              table + pushed_predicates   │
│                                          │
│  Cache hit? → use it                     │
│  Cache miss? → call SaaS API             │
│                                          │
│  Returns: BROAD dataset (all tenant data)│
└──────────────┬───────────────────────────┘
               │
               ▼
┌──────────────────────────────────────────┐
│  Post-Fetch Pipeline (all local)         │
│  1. Apply user's WHERE filters           │
│  2. Apply RLS (row filtering)            │  ← security enforcement here
│  3. Apply CLS (column masking)           │  ← security enforcement here
│  4. Apply ORDER BY / LIMIT               │
│  5. Return to user                       │
└──────────────────────────────────────────┘
```

Cache strategy and entitlements don't conflict — they operate at different stages:
- **Cache optimizes the fetch** (tenant-level, broad)
- **Entitlements secure the response** (user-level, narrow)

---

## Summary

| Aspect | Decision |
|---|---|
| **Source of truth** | Always the SaaS API. Cache never overrides it. |
| **Cache granularity** | Pushed predicates (what we asked the API), not full SQL. |
| **Hit rate strategy** | Push fewer predicates, filter locally. Predicate subsumption for broader matches. |
| **Freshness control** | `max_staleness` hint, clamped by connector floor. ETag for cheap revalidation. |
| **Abuse protection** | Floor staleness + per-tenant live-fetch budget. Transparent `CACHE_FORCED` fallback. |
| **Join strategy** | Each side cached independently. Join computed locally from cached fetches. |
| **Data retention** | In-memory only. TTL + LRU eviction. No persistence. No compliance burden. |
| **Entitlements** | RLS/CLS applied post-fetch on cached data. Cache is tenant-scoped, not user-scoped. |
| **Transparency** | Every response carries `freshness_ms`, `freshness_source`, and `rate_limit_status`. |
