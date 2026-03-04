# Data Plane — Comprehensive Internal Design

## Table of Contents

1. [Data Plane Architecture Overview](#1-data-plane-architecture-overview)
2. [Connector Deployment Topology: In-Process vs Out-of-Process](#2-connector-deployment-topology-in-process-vs-out-of-process)
3. [Query Parsing Pipeline](#3-query-parsing-pipeline)
4. [Query Planner ↔ Connector Integration](#4-query-planner--connector-integration)
5. [Connector Isolation & Fault Domains](#5-connector-isolation--fault-domains)
6. [Cache Topology (Redis Cluster)](#6-cache-topology-redis-cluster)
7. [Memory Management & Spill Strategy](#7-memory-management--spill-strategy)
8. [Join Execution Engine](#8-join-execution-engine)
9. [Async vs Sync Execution Paths](#9-async-vs-sync-execution-paths)
10. [End-to-End Data Flow (Putting It All Together)](#10-end-to-end-data-flow-putting-it-all-together)
11. [Prototype vs Production Gap](#11-prototype-vs-production-gap)
12. [Goroutine Pool Provisioning & Resource Sizing](#12-goroutine-pool-provisioning--resource-sizing)
13. [Memory Accounting — Practical Implementation](#13-memory-accounting--practical-implementation)

---

## 1. Data Plane Architecture Overview

The data plane is the hot path. Every user query flows through it. The control plane (tenant registry, policy store, schema catalog) is the configuration layer that feeds the data plane — but the data plane does all the real-time work.

### Component Map

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
│  │                 │   │                   │   │                    │     │
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
│  │  • Source Cache (Redis / in-process)                                │    │
│  │  • Rate-Limit Service (token buckets, semaphores)                   │    │
│  │  • Entitlement Engine (RLS/CLS)                                    │    │
│  │  • Materialization Store (S3/DuckDB for spill + large joins)       │    │
│  └────────────────────────────────────────────────────────────────────┘    │
└─────────────────────────────────────────────────────────────────────────────┘
```

### Key Principle: The Executor Is the Conductor

The query executor is the most important runtime component. It orchestrates everything:
- It decides *when* to call connectors (after entitlement checks pass)
- It decides *how* to call them (concurrently via errgroup, with timeout context)
- It decides *what to do with results* (join, filter, project, or spill to disk)
- It owns the memory budget and decides when to switch from in-memory to S3-backed execution

The planner is a **pure function**: SQL string in, QueryPlan struct out. No side effects, no I/O.
The executor is the **stateful orchestrator**: QueryPlan in, rows out, with all the messy real-world concerns (rate limits, cache, timeouts, memory pressure).

### OPA Sidecar — Authorization Boundary

The OPA sidecar runs as a separate container in the same pod. It owns exactly one decision: **"can this user access this table?"** — a binary allow/deny evaluated at plan time, before any connector is called.

**What OPA does**: table-level authorization (binary).
**What OPA does not do**: column masking, row filtering. Those are executor responsibilities applied post-fetch (see "Post-fetch RLS/CLS" in the executor box above).

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

The call is loopback-local (< 1ms). The OPA sidecar holds the full policy bundle in-memory and is kept current by OPAL push from the control plane (~1-2s propagation on revocation). See control-plane-design-notes.md § Step 4 for OPAL propagation flow, bundle structure, and sidecar resource sizing.

---

## 2. Connector Deployment Topology: In-Process vs Out-of-Process

This is the fundamental architectural decision you're wrestling with. Let's be rigorous about it.

### Option A: In-Process Connectors (What the Prototype Does)

```
┌──────────────────────────────────────────────────┐
│  Query Gateway Pod                               │
│                                                  │
│  ┌──────────────────────────────────────┐        │
│  │  Go process                          │        │
│  │                                      │        │
│  │  HTTP handler                        │        │
│  │    → Parser                          │        │
│  │    → Executor                        │        │
│  │        → GitHub Connector (goroutine)│        │
│  │        → Jira Connector (goroutine)  │        │
│  │        → Join + filter               │        │
│  │    → Response                        │        │
│  └──────────────────────────────────────┘        │
└──────────────────────────────────────────────────┘
```

**How data flows**: Connector.Fetch() returns `[]Row` directly in-process. No serialization, no network hop. The executor accesses the returned rows in the same heap.

**Pros**:
- Zero serialization overhead — rows are native Go maps/structs in shared memory
- Sub-microsecond "data transfer" between connector and executor
- Simple deployment — one binary, one pod, one scaling unit
- Easy debugging — single process, one set of goroutine dumps, one pprof endpoint
- Go's goroutine model gives natural concurrency without thread management

**Cons**:
- A panicking connector crashes the entire gateway process
- A connector with a memory leak degrades all other connectors in the same process
- No independent scaling — can't scale Jira connectors without scaling GitHub connectors
- A CPU-bound connector starves the goroutine scheduler for other connectors
- All connectors share the same Go process memory limit (GOMEMLIMIT)

### Option B: Out-of-Process Connectors (Separate Microservices)

```
┌───────────────────────────────┐
│  Query Gateway Pod            │
│  ┌─────────────────────────┐  │
│  │  HTTP handler           │  │
│  │    → Parser             │  │
│  │    → Executor           │  │
│  │        → gRPC call ──────────► GitHub Connector Pod (separate k8s deployment)
│  │        → gRPC call ──────────► Jira Connector Pod (separate k8s deployment)
│  │        → Join + filter  │  │
│  │    → Response           │  │
│  └─────────────────────────┘  │
└───────────────────────────────┘
```

**How data flows**: Executor calls `connector.Fetch()` over gRPC (or HTTP). Connector serializes result rows to protobuf/JSON, sends them over the network. Executor deserializes them back into Go structs/maps, then proceeds with join and filtering.

**Pros**:
- **Fault isolation**: A crashing Jira connector doesn't take down the gateway or GitHub connector
- **Independent scaling**: Scale Jira connector pods independently based on Jira API usage
- **Memory isolation**: Each connector has its own memory limit (k8s resource limits). A connector fetching a huge payload can OOMKill itself without affecting others
- **Independent deployment**: Ship a connector fix without redeploying the gateway
- **Language flexibility**: Connectors don't have to be Go (useful if a vendor provides a Python SDK)

**Cons**:
- Serialization cost: Every row crosses a protobuf encode → network → decode boundary
- Network latency: Even in the same k8s cluster, adds 0.5-2ms per call (pod-to-pod)
- Operational complexity: N more deployments, N more scaling configs, N more health checks
- **Data transfer for joins**: For a JOIN query, rows from BOTH connectors must traverse the network to reach the executor, where the join happens. This is the pain point you identified.

### The Decision: **In-Process with Bulkhead Isolation** (Right Answer for This Scale)

Here's why this is the correct call, backed by how Trino, Presto, and Dremio all handle it:

**The core insight**: In a federated query engine, the *same process* that calls the connector also needs to *join and filter the results*. Separating the connector into a different process means serializing the entire result set, sending it over the network, and deserializing it — only to then process it in the executor. For a join of 10,000 rows × 10,000 rows, that's 20K rows serialized twice (once from each connector to the executor). The join itself is O(n) with a hash join. **The network transfer dominates the join cost.**

Trino, Presto, and Dremio all run connectors in-process as plugins/modules. The "connector" is a Java class loaded in the same JVM as the query engine. Data never leaves the process for the connector → executor path.

**However**, in-process doesn't mean unprotected. We use **bulkhead isolation** patterns:

```
┌────────────────────────────────────────────────────────────────────────┐
│  Query Gateway Pod (single Go process)                                │
│                                                                       │
│  ┌────────────────────────────────────────────────────────────────┐   │
│  │  Executor                                                      │   │
│  │                                                                │   │
│  │  ┌───────────────────┐   ┌───────────────────┐                │   │
│  │  │ GitHub Bulkhead   │   │ Jira Bulkhead     │                │   │
│  │  │                   │   │                   │                │   │
│  │  │ • Goroutine pool: │   │ • Goroutine pool: │                │   │
│  │  │   max 50          │   │   max 30          │                │   │
│  │  │ • Memory budget:  │   │ • Memory budget:  │                │   │
│  │  │   200MB           │   │   150MB           │                │   │
│  │  │ • Timeout: 8s     │   │ • Timeout: 10s    │                │   │
│  │  │ • Circuit breaker │   │ • Circuit breaker │                │   │
│  │  │   (5 failures →   │   │   (5 failures →   │                │   │
│  │  │    open for 30s)  │   │    open for 30s)  │                │   │
│  │  └───────────────────┘   └───────────────────┘                │   │
│  └────────────────────────────────────────────────────────────────┘   │
│                                                                       │
│  Pod memory limit: 2GB      GOMEMLIMIT: 1.8GB                        │
└────────────────────────────────────────────────────────────────────────┘
```

### Bulkhead Isolation Mechanisms (In-Process)

#### 1. Goroutine Pool per Connector

Each connector gets a bounded goroutine pool (a semaphore). This prevents one connector from spawning unlimited goroutines (e.g., 10,000 concurrent paginated fetches) and starving the Go scheduler.

```go
type ConnectorBulkhead struct {
    connector  connectors.Connector
    sem        chan struct{}  // bounded goroutine pool
    memBudget  int64         // bytes
    memUsed    atomic.Int64
    timeout    time.Duration
    breaker    *CircuitBreaker
}

func (b *ConnectorBulkhead) Fetch(ctx context.Context, principal *models.Principal, sq models.SourceQuery) ([]models.Row, models.SourceMeta, error) {
    // Goroutine pool admission
    select {
    case b.sem <- struct{}{}:
        defer func() { <-b.sem }()
    case <-ctx.Done():
        return nil, models.SourceMeta{}, ctx.Err()
    }

    // Circuit breaker check
    if !b.breaker.Allow() {
        return nil, models.SourceMeta{}, qerrors.New(
            qerrors.CodeSourceTimeout,
            fmt.Sprintf("connector %s circuit open: too many recent failures", b.connector.ID()),
            b.connector.ID(), 30, nil,
        )
    }

    // Timeout enforcement
    fetchCtx, cancel := context.WithTimeout(ctx, b.timeout)
    defer cancel()

    rows, meta, err := b.connector.Fetch(fetchCtx, principal, sq)
    if err != nil {
        b.breaker.RecordFailure()
        return nil, meta, err
    }
    b.breaker.RecordSuccess()

    // Memory accounting (post-fetch, approximate)
    rowBytes := estimateRowBytes(rows)
    if b.memUsed.Add(rowBytes) > b.memBudget {
        b.memUsed.Add(-rowBytes)
        return nil, meta, qerrors.New(
            qerrors.CodeSourceTimeout,
            fmt.Sprintf("connector %s exceeded memory budget", b.connector.ID()),
            b.connector.ID(), 0, nil,
        )
    }
    // Release memory accounting after rows are consumed by the executor
    defer b.memUsed.Add(-rowBytes)

    return rows, meta, nil
}
```

#### 2. Circuit Breaker per Connector

If a connector fails 5 times in 60 seconds, the circuit opens. Subsequent requests immediately return an error for 30 seconds without calling the connector. This prevents a failing connector from:
- Burning rate-limit budget on requests that will fail anyway
- Causing timeout cascades that slow down healthy connectors
- Filling error logs with repeated failure traces

```
CLOSED (normal) ─── 5 failures in 60s ───► OPEN (reject all)
     ▲                                            │
     │                                     30s cooldown
     │                                            │
     └─── success ◄── HALF-OPEN (allow 1 probe) ◄┘
```

This is the same pattern Netflix Hystrix popularized, now standard in any distributed system. In Go, `sony/gobreaker` or a simple hand-rolled implementation works.

#### 3. Per-Connector Timeout

Each connector gets its own timeout, independent of the query-level timeout. The query-level timeout is the outer bound; the connector timeout is the inner bound. This prevents a single slow connector from consuming the entire query timeout budget, leaving no time for the other connector to respond.

```
Query timeout: 10s
├── GitHub connector timeout: 5s (fast API, shouldn't need more)
├── Jira connector timeout: 8s (JQL can be slow)
└── Reserved for join + post-processing: 2s minimum
```

If GitHub times out at 5s, the executor can still get Jira results within the remaining 5s and return a partial result (or error, depending on the query type).

#### 4. Memory Budget per Connector

This is the hardest isolation to achieve in-process. Go doesn't have per-goroutine memory limits. The approach:

- **Approximate accounting**: After `Fetch()` returns, estimate the memory footprint of the returned `[]Row` (row count × average row size). Track this against a per-connector budget.
- **Reject if over budget**: If a connector returns a result set that would push its cumulative memory usage past its budget, reject the response and return an error to the executor.
- **Process-level protection**: Set `GOMEMLIMIT` at the process level. If total memory across all connectors exceeds this, the Go GC runs more aggressively. If that's not enough, the OOMKiller takes the pod and k8s restarts it.

**Why approximate is okay**: We don't need byte-exact accounting. We need to prevent catastrophic memory exhaustion. If a connector returns 50MB of rows when its budget is 200MB, we're fine. If it returns 500MB, we catch it and reject before the process OOMs. The precision of `estimateRowBytes` (count × average bytes per row) is sufficient for this purpose.

### When to Consider Out-of-Process (Later, Conditional)

Move a connector out-of-process only when:

| Trigger | Why | Example |
|---|---|---|
| Connector needs a different language runtime | Vendor provides only a Python/Java SDK | SAP, Oracle |
| Connector has known memory-safety issues | Third-party code that leaks or corrupts memory | Untrusted community connectors |
| Connector scales wildly differently | 100x more traffic to one connector than others | A "data lake" connector vs API connectors |
| Regulatory requirement | Certain data must be processed in an isolated compute boundary | PCI-DSS, HIPAA connectors |

Even then, the out-of-process connector communicates via gRPC and streams rows (not batch-sends them), which amortizes the serialization cost.

### Industry Precedent

| Engine | Connector Model | Notes |
|---|---|---|
| **Trino (f.k.a. PrestoSQL)** | In-process Java plugin | Connectors are SPI implementations loaded in the same JVM. No network hop for connector → engine data transfer. |
| **Presto** | In-process Java plugin | Same model as Trino (forked from same codebase). |
| **Apache Calcite** | In-process adapter | Adapters implement the `Schema` interface. All in-JVM. |
| **Dremio** | In-process plugin | Source plugins run in the executor's JVM. |
| **DuckDB** | In-process extension | Extensions are loaded as shared libraries into the same process. |
| **Google BigQuery** | Out-of-process worker | Different model: workers fetch data from GCS/Bigtable and shuffle over network. But BigQuery processes petabytes — the scale justifies the network overhead. |

**Pattern**: All federated query engines at our scale target (1k QPS, 100MB/s) use in-process connectors. Out-of-process is only used at BigQuery/Snowflake scale (PB-scale, thousands of worker nodes).

---

## 3. Query Parsing Pipeline

### 3.1 Overview

The parsing pipeline transforms a SQL string into a `QueryPlan` struct — a fully-resolved execution plan that the executor consumes without touching SQL again.

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
│  → Consult capability model*    │
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

*In the prototype, the filter classifier pushes all single-table predicates to the connector. In production, it consults the connector's capability declaration (which columns/ops are pushdown-eligible) and only pushes what the connector can handle server-side.

### 3.2 CPU Characteristics

SQL parsing is CPU-bound but fast. The vitess-sqlparser is a Go port of MySQL's parser — parsing a typical query (the 5-line demo query) takes **~50-100μs**. Even a complex query with 10 JOINs and 20 WHERE clauses parses in under 1ms.

At 1k QPS, parsing consumes approximately:

```
1000 queries/sec × 100μs/query = 100ms of CPU time per second = ~10% of one core
```

This is negligible. Parsing is not the bottleneck and does not need to be parallelized, offloaded, or cached. The real latency is in connector fetches (network I/O to SaaS APIs).

### 3.3 The QueryPlan Contract

The QueryPlan struct is the interface between the planner and the executor. It captures every decision the planner made:

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
    ConnectorID string         // "github", "jira"
    Table       string         // "github.pull_requests"
    Alias       string         // "gh"
    Filters     []FilterExpr   // pushed down to this connector
    Limit       int            // propagated from outer LIMIT
}
```

**Design invariant**: The executor never looks at the `SQL` field. It only consumes the structured fields. This means a different planner (e.g., cost-based optimizer, ML-assisted) can emit the same struct. The planner and executor are cleanly separated by this struct boundary.

### 3.4 Production Parser Evolution

The prototype uses `vitess-sqlparser` (MySQL dialect). For production:

| Aspect | Prototype | Production |
|---|---|---|
| **Parser** | vitess-sqlparser (MySQL) | ANTLR4-based custom grammar |
| **Dialect** | MySQL subset | Custom SQL dialect with extension points |
| **Extensions** | None | `UNNEST`, `LATERAL JOIN`, staleness hints inline (`/*+ MAX_STALENESS(5s) */`) |
| **Validation** | Basic (SELECT-only) | Schema-aware validation (column existence, type compatibility) |
| **Cost estimation** | None | Cardinality estimates from catalog statistics |

The critical point: the parser is a pure function. Swapping implementations doesn't affect the executor, cache, rate-limiter, or any downstream component.

---

## 4. Query Planner ↔ Connector Integration

### 4.1 The Interface Boundary

The connector interface is deliberately minimal:

```go
type Connector interface {
    ID()     string
    Tables() []string
    Schema(table string) ([]Column, error)
    Fetch(ctx context.Context, principal *Principal, sq SourceQuery) ([]Row, SourceMeta, error)
}
```

The planner never calls connectors directly. It emits a `QueryPlan`, and the **executor** calls connectors. The planner only needs connector metadata (schema, capabilities) — not runtime execution.

```
Planner reads:                    Executor calls:
  • Schema Catalog (schemas)        • Connector.Fetch()
  • Capability model (pushdown)     • Rate-Limit Service
  • (cached from control plane)     • Cache
                                    • Entitlement Engine
```

This separation is important. The planner is a **read-only, stateless** computation. The executor is a **stateful orchestration** that interacts with external systems (SaaS APIs via connectors, Redis for cache, rate-limit state).

### 4.2 Connector Query Translation (SourceQuery → SaaS API Language)

The connector **never receives raw SQL**. It receives a `SourceQuery` struct — a normalized, language-agnostic representation of what to fetch. The connector's sole translation responsibility is converting this into whatever the SaaS API speaks:

```
SourceQuery                          Connector translates to SaaS language
{                                    ─────────────────────────────────────
  ConnectorID: "jira",               Jira:       JQL → assignee = alice AND status = open
  Table: "jira.issues",              GitHub:     REST params → ?state=open&per_page=100
  Filters: [                         Salesforce: SOQL → SELECT Id FROM Case WHERE OwnerId = 'alice'
    {assignee = alice},              Notion:     JSON filter body
    {status = open}                  Google:     API-specific query params
  ],
  Limit: 100,
}
```

Each SaaS API has a completely different query language. That translation is entirely encapsulated inside the connector. The planner and executor know nothing about JQL, SOQL, or GraphQL.

| Component | Knows about | Doesn't know about |
|---|---|---|
| **Planner** | SQL syntax, table aliases, filter classification | JQL, SOQL, GitHub API params |
| **Executor** | `SourceQuery` struct, cache keys, rate limits | JQL, SOQL, GitHub API params |
| **Connector** | SaaS API language, pagination, auth tokens | SQL, other connectors, join logic |

Adding a new SaaS connector means writing one query-translation function and one response-transformation function. Zero changes to the planner or executor.

### 4.3 How the Executor Invokes Connectors

The core execution loop:

```
For each SourceQuery in plan.Sources (concurrent via errgroup):

  1. Entitlement pre-check
     │
  2. Rate-limit reservation (L3 + L4)
     │
  3. Cache lookup (pushed predicate key)
     ├── HIT  → cancel rate-limit reservation, return cached rows
     └── MISS → proceed to step 4
     │
  4. Connector.Fetch(ctx, principal, sourceQuery)
     │
  5. Cache write (store result with TTL)
     │
  6. Return rows to executor's merge phase
```

Steps 1-6 run in parallel for each source. The `errgroup` provides structured cancellation: if any source fails fatally, siblings are cancelled via shared context.

### 4.4 The Data Return Path

This is where your connector-as-separate-service concern comes in. Let's trace the data flow:

**In-process (current)**:
```
Connector.Fetch() → returns []Row (Go slice, same heap) → executor has rows → join
Total cost: 0 (no serialization, no copy, same memory space)
```

**Out-of-process (hypothetical)**:
```
Connector Pod receives gRPC request
  → Calls SaaS API → gets JSON response → deserializes to internal structs
  → Serializes to protobuf → sends over gRPC to executor pod
  → Executor deserializes protobuf → creates []Row → join

Total cost per row: ~2μs serialize + ~0.5ms network RTT + ~2μs deserialize
For 10,000 rows: ~40ms serialization + ~0.5ms network = ~40.5ms overhead
```

For a single-source query returning 1,000 rows, the overhead is ~5ms — acceptable. But for a JOIN query where both sides return 10,000 rows, it's ~80ms of pure serialization overhead on top of the actual SaaS API latency. When your P50 target is 500ms, burning 80ms on serialization is 16% of the budget for zero functional benefit.

### 4.5 Streaming vs Batch Data Return

In the prototype, `Fetch()` returns all rows in one shot (`[]Row`). This is fine for small-to-medium result sets but becomes a memory problem for large ones.

**Production evolution**: The connector interface should support streaming:

```go
type StreamingConnector interface {
    Connector

    // FetchStream returns a channel of row batches.
    // Each batch is ~1000 rows. The executor processes batches as they arrive.
    // The channel is closed when all rows have been sent.
    FetchStream(ctx context.Context, principal *Principal, sq SourceQuery) (<-chan RowBatch, error)
}

type RowBatch struct {
    Rows  []Row
    Meta  SourceMeta
    Final bool  // true if this is the last batch
}
```

Benefits of streaming:
- **Memory bounded**: The executor processes and discards batches. It doesn't need to hold the entire result set in memory.
- **Pipeline parallelism**: While the connector is fetching page 2 from the SaaS API, the executor is already processing page 1 (applying RLS, building the hash table for join).
- **Early termination**: For a `LIMIT 10` query, the executor can cancel the stream after receiving 10 matching rows without waiting for the connector to fetch all pages.

The prototype uses batch `Fetch()` because the mock data is ~200 rows. Streaming is a production optimization for result sets >10K rows.

### 4.6 Capability-Driven Predicate Pushdown

The planner's most important optimization is deciding which predicates to push to the connector. This is driven by the connector's declared capabilities:

```yaml
connector: jira
tables:
  issues:
    pushdown_ops: ["=", "!=", "in"]
    pushdown_columns: ["assignee", "status", "project", "priority"]
    max_page_size: 100
```

```
User query: WHERE j.assignee = 'alice' AND j.priority = 'high' AND j.created > '2024-01-01'

Planner checks Jira capabilities:
  assignee = 'alice'       → pushdown_columns includes "assignee", op "=" supported → PUSHDOWN
  priority = 'high'        → pushdown_columns includes "priority", op "=" supported → PUSHDOWN
  created > '2024-01-01'   → pushdown_columns does NOT include "created"            → POST-FETCH

SourceQuery sent to Jira: {Filters: [{assignee = 'alice'}, {priority = 'high'}]}
PostFilters applied locally: [{created > '2024-01-01'}]
```

**Trade-off the planner makes**:
- Push MORE predicates → Jira returns fewer rows → less data to transfer and filter locally → BUT narrower cache key → lower cache reuse
- Push FEWER predicates → Jira returns more rows → more data → BUT broader cache key → higher cache reuse across similar queries

The prototype pushes all supported predicates. A production cost-based optimizer would reason about expected selectivity and cache reuse.

---

## 5. Connector Isolation & Fault Domains

### 5.1 What We're Protecting Against

| Fault | Impact without isolation | Mitigation |
|---|---|---|
| Connector panics (nil pointer, etc.) | Crashes the entire gateway process | `recover()` in the bulkhead wrapper |
| Connector hangs (SaaS API unresponsive) | Consumes a goroutine forever, eventually exhausts goroutine pool for all connectors | Per-connector timeout + bounded goroutine pool |
| Connector returns huge result | OOMs the process, killing all in-flight queries | Memory budget per connector + GOMEMLIMIT |
| Connector has a bug that leaks goroutines | Slow memory/CPU degradation over hours | Goroutine pool bound + Prometheus goroutine gauge + auto-restart via k8s liveness probe |
| SaaS API returns malformed data | Connector JSON unmarshalling panics or spins | `recover()` + timeout |
| Connector makes too many API calls | Burns rate-limit budget for everyone | Rate-limit service (L3/L4) + circuit breaker |

### 5.2 Bulkhead Implementation (Detailed)

```go
type ConnectorBulkhead struct {
    connector   connectors.Connector
    sem         chan struct{}     // goroutine pool
    maxMemBytes int64
    memUsed     atomic.Int64
    timeout     time.Duration
    breaker     *CircuitBreaker
    logger      *zap.Logger
    metrics     *BulkheadMetrics
}

func NewBulkhead(c connectors.Connector, cfg BulkheadConfig) *ConnectorBulkhead {
    return &ConnectorBulkhead{
        connector:   c,
        sem:         make(chan struct{}, cfg.MaxConcurrency),
        maxMemBytes: cfg.MaxMemoryBytes,
        timeout:     cfg.Timeout,
        breaker:     NewCircuitBreaker(cfg.BreakerThreshold, cfg.BreakerCooldown),
        logger:      cfg.Logger.With(zap.String("connector", c.ID())),
    }
}

func (b *ConnectorBulkhead) Fetch(ctx context.Context, principal *models.Principal, sq models.SourceQuery) (rows []models.Row, meta models.SourceMeta, err error) {
    // Panic recovery — a panicking connector must not crash the process
    defer func() {
        if r := recover(); r != nil {
            err = qerrors.New(qerrors.CodeSourceTimeout,
                fmt.Sprintf("connector %s panicked: %v", b.connector.ID(), r),
                b.connector.ID(), 0, nil)
            b.breaker.RecordFailure()
            b.metrics.PanicsTotal.Inc()
        }
    }()

    // Circuit breaker
    if !b.breaker.Allow() {
        return nil, models.SourceMeta{}, qerrors.New(qerrors.CodeSourceTimeout,
            fmt.Sprintf("connector %s circuit open", b.connector.ID()),
            b.connector.ID(), 30, nil)
    }

    // Goroutine pool admission (bounded concurrency)
    select {
    case b.sem <- struct{}{}:
        defer func() { <-b.sem }()
    case <-ctx.Done():
        return nil, models.SourceMeta{}, ctx.Err()
    }

    // Per-connector timeout (inner bound, within query timeout)
    fetchCtx, cancel := context.WithTimeout(ctx, b.timeout)
    defer cancel()

    rows, meta, err = b.connector.Fetch(fetchCtx, principal, sq)
    if err != nil {
        b.breaker.RecordFailure()
        return nil, meta, err
    }
    b.breaker.RecordSuccess()

    // Memory accounting
    rowBytes := estimateRowBytes(rows)
    if b.memUsed.Add(rowBytes) > b.maxMemBytes {
        b.memUsed.Add(-rowBytes)
        return nil, meta, qerrors.New(qerrors.CodeSourceTimeout,
            fmt.Sprintf("connector %s result exceeds memory budget (%dMB)",
                b.connector.ID(), b.maxMemBytes/(1024*1024)),
            b.connector.ID(), 0, nil)
    }

    return rows, meta, nil
}
```

### 5.3 Panic Recovery — Why It Matters

Go's `recover()` only works in the same goroutine that panicked. Since each connector runs in its own goroutine (via `errgroup`), we wrap the `Fetch()` call with a deferred `recover()` in the bulkhead. This ensures:

- A nil-pointer dereference inside the GitHub connector returns a `SOURCE_TIMEOUT` error to the executor
- The executor handles it like any other connector error (returns error or partial result)
- The process continues serving other queries

Without this, a single panicking connector takes down the pod, triggering a k8s restart and dropping all in-flight queries — a much worse outcome.

### 5.4 Connector as a Standardized Interface — Isolation Benefit You Identified

You're right that the key isolation benefit is: **a faulty connector doesn't break the entire system**. This works because:

1. Every connector implements the same 4-method interface
2. The executor treats connectors as opaque — it doesn't know or care about their internals
3. The bulkhead wraps every connector uniformly
4. A new connector can be added without touching the executor, planner, gateway, or any other code

```
New connector code → implements Connector interface → registered in Registry
                                    │
                                    ▼
Bulkhead wraps it automatically:
  • Goroutine pool ✓
  • Timeout ✓
  • Circuit breaker ✓
  • Panic recovery ✓
  • Memory accounting ✓

Zero changes to: executor, planner, gateway, cache, rate-limiter
```

This is the plugin architecture benefit. Each connector is an isolated unit of risk. Its blast radius is bounded by the bulkhead. Its retry logic, pagination, auth refresh, and error handling are all encapsulated within the connector itself.

---

## 6. Cache Topology (Redis Cluster)

### 6.1 Production Cache: Redis Cluster, Not Standalone

The prototype uses an in-process Go map. Production uses Redis Cluster (6+ nodes, 16384 hash slots). Cache keys are distributed across nodes via consistent hashing.

```
Cache key = SHA256(tenant + connector + table + pushed_predicates)
  → hashes to a slot (0-16383)
  → slot maps to one specific Redis node
  → one GET, one node, one round trip (~0.5ms)
```

### 6.2 Distribution Strategy: Scattered, Not Connector-Affine

**Do NOT pin connectors to specific Redis nodes** (e.g., "all GitHub cache on node 1"). That creates hot-node problems when one connector is popular. Instead, let consistent hashing scatter keys naturally.

This works because different queries produce different cache keys:
- Different tenants → different hash
- Different pushed predicates → different hash
- Same tenant + same connector + same filters → same hash (intentional: cache hit)

The result: cache load distributes evenly across nodes even if one connector gets 80% of traffic, because different tenants and filter combinations spread across different hash slots.

### 6.3 Fetch Is Always Single-Key

Because the cache key hashes the full pushed predicate set into one key, a cache lookup is always one `GET` to one node. No scatter-gather.

```
Single-source query: 1 Redis GET → 1 node  → ~0.5ms
JOIN query:          2 Redis GETs → 1-2 nodes → ~1ms (pipelined)
```

For JOINs, the two source cache lookups can be pipelined (send both GETs before waiting for either response). Even if they land on different Redis nodes, total latency is ~1ms — negligible vs SaaS API latency (50-800ms).

### 6.4 Hot-Key Risk

The only hot-key scenario: thousands of concurrent requests for the exact same `(tenant + connector + table + filters)`. But this is a cache *hit* — Redis handles millions of reads/sec per node, so a hot read key isn't a problem. It only becomes a concern if the cached value is very large (>1MB), in which case network bandwidth from that Redis node could saturate. For very large cached results, the source cache stores a pointer to S3, not the data itself.

### 6.5 TTL Strategy: Soft TTL + Hard TTL + ETag Revalidation

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

#### Revalidation Flow (for ETag-capable connectors)

```
t=0        → cache miss → fetch → store rows + ETag; set soft_ttl=+5m, hard_ttl=+30m
t < 5min   → soft TTL fresh   → serve immediately, no background work
t = 6min   → soft TTL expired → serve stale rows + fire ONE background revalidation goroutine
               background: GET with If-None-Match: "<etag>"
                 → 304 Not Modified → reset soft_ttl=+5m, keep rows (data unchanged)
                 → 200 with new body → update rows + new ETag, reset both TTLs
t = 31min  → hard TTL expired → synchronous re-fetch; user waits
```

The requesting user at `t=6min` sees zero added latency — they get stale data while revalidation happens behind the scenes. Stale data is at most `(time since soft TTL expired)` old, typically seconds to low minutes.

For connectors without ETag support, soft TTL and hard TTL collapse to the same value — there is no cheap way to extend freshness without a full re-fetch.

#### Thundering Herd Prevention: singleflight

If 500 concurrent requests all observe an expired soft TTL for the same key, only **one** background revalidation goroutine should fire. Go's `singleflight` deduplicates in-flight revalidations per cache key:

```go
var revalidationGroup singleflight.Group

// Only one goroutine fires per key; the other 499 serve stale and return immediately
go revalidationGroup.Do(cacheKey, func() (interface{}, error) {
    return revalidate(cacheKey, entry.ETag)
})
```

Without this, 500 goroutines would each fire a conditional GET — burning 500 rate-limit tokens for what should be one API call.

#### Rate-Limit Gate on Revalidation

Even a 304 response consumes a rate-limit token on most SaaS APIs. The background revalidation goroutine checks the rate-limit service before firing. If the tenant's budget is constrained, it silently extends the soft TTL instead of burning a token:

```go
func revalidate(key string, etag string) {
    if !rateLimiter.TryAcquire(connectorID, tenantID) {
        // Budget tight — extend soft TTL, don't waste a token
        entry.SoftTTL = time.Now().Add(softTTLDuration)
        return
    }
    // Fire conditional GET with If-None-Match: etag
    // ...
}
```

---

## 7. Memory Management & Spill Strategy

### 7.1 The Memory Problem

The data plane processes rows in memory. For small result sets (hundreds to low thousands of rows), this is fine. But the system needs to handle:

- A connector returning 100K rows (user queried a large Jira project with no filter)
- A JOIN producing a cross-product of 10K × 10K = 100M candidate pairs (before the join predicate filters most of them)
- Multiple concurrent queries, each with medium-sized result sets, whose aggregate memory exceeds the pod budget

### 7.2 Memory Budget Architecture

```
Pod memory: 4GB
├── Go runtime + GC overhead:    500MB
├── Cache (Redis/in-process):    1GB (configurable via CACHE_MAX_MEM)
├── Connector memory budgets:    1.5GB total
│   ├── GitHub:    300MB
│   ├── Jira:      300MB
│   ├── Salesforce: 300MB
│   ├── Shared/other: 600MB
├── Executor working memory:     800MB (join buffers, sort buffers)
└── Safety margin:               200MB (OOM headroom)
```

These are soft limits enforced by the application. The hard limit is the k8s pod memory limit. `GOMEMLIMIT` is set to ~80% of the pod limit to give the GC room to work.

### 7.3 Spill-to-Disk Strategy

When in-memory processing would exceed the budget, the system spills to disk (local SSD or S3).

#### When to Spill

| Condition | Metric | Action |
|---|---|---|
| Single connector returns >50K rows | Row count in Fetch response | Write to Parquet on local disk; stream to executor |
| Hash join build side >100K rows | Row count in hash table | Build hash table on DuckDB (disk-backed) instead of in-memory map |
| Process memory >70% of GOMEMLIMIT | `runtime.MemStats.HeapInuse` | Trigger forced spill for all active queries; switch to disk-backed execution |
| Query result set >10MB | Estimated bytes of projected rows | Write result to S3; return a presigned URL instead of inline rows |

#### Spill Execution Flow

```
Normal (in-memory) path:
  Connector.Fetch() → []Row → hash join in-memory → project → return inline

Spill path (triggered by size or memory pressure):
  Connector.Fetch() → []Row → exceeds threshold
    → Write rows to local Parquet file (via DuckDB or parquet-go)
    → Hash join executed by DuckDB on the Parquet files
    → Result set written to new Parquet file
    → If result small enough → read back into memory → return inline
    → If result still large → upload to S3 → return presigned URL
```

#### DuckDB as the Spill Engine

DuckDB is an **embeddable columnar database library** — same category as SQLite, but optimized for analytics instead of OLTP.

For this design we use a **hybrid mode**:
- **Sync/small joins**: in-process via CGo (`github.com/marcboeker/go-duckdb`) for lowest latency.
- **Async/spill-heavy joins**: persistent sidecar process (same pod, shared local disk) for memory isolation.

No Spark/Hadoop cluster is required at this scale.

```
SQLite  = embedded row-store  (OLTP, transactions)
DuckDB  = embedded column-store (OLAP, analytics, Parquet-native)
```

**Why DuckDB and not Spark/Flink?** Spark is a distributed cluster engine for petabyte-scale. We're joining two files on a single node. DuckDB does the same join in-process in milliseconds-to-seconds. You'd only need Spark if joining billions of rows across multiple machines — far beyond our scale target.

**How the spill works in practice (async/large path)**:

```
1. Connector returns 200K rows → exceeds memory threshold
2. Write rows to /tmp/query-{trace_id}-left.parquet     (~50ms, parquet-go library)
3. Write other side to /tmp/query-{trace_id}-right.parquet
4. DuckDB sidecar (same pod, persistent process):
     db.Exec("SELECT * FROM '/tmp/...-left.parquet' l
              JOIN '/tmp/...-right.parquet' r ON l.key = r.key")
5. DuckDB handles the join using its own disk-backed execution engine
   → it has its own configurable memory limit (e.g., 512MB)
   → if that's exceeded, DuckDB spills its own intermediates to disk
6. Result small → read back into Go []Row → return inline
   Result large → write to S3 → return presigned URL
7. Delete temp Parquet files (deferred cleanup)
```

DuckDB's key properties for this use case:
- **Parquet-native**: Reads/writes Parquet without conversion. Column pruning and predicate pushdown on Parquet files out of the box.
- **Bounded memory**: Has its own memory manager. Configurable limit (e.g., `SET memory_limit='512MB'`). Spills to disk when exceeded.
- **Operationally lightweight**: Works either embedded (CGo) or as a tiny sidecar in the same pod.

```
┌────────────────────────────────────────────────────────────────┐
│  In-Memory Path (default, <50K rows per side)                  │
│  Connector → []Row → hashJoin() in Go → project → return      │
│  Latency: <100ms    Memory: proportional to result size        │
└────────────────────────────────────────────────────────────────┘

┌────────────────────────────────────────────────────────────────┐
│  Spill Path (triggered by size/memory, >50K rows per side)     │
│  Connector → []Row → write Parquet → DuckDB sidecar join       │
│  → result Parquet → read back or S3 upload                     │
│  Latency: 1-5s     Memory: bounded by DuckDB's own limit      │
└────────────────────────────────────────────────────────────────┘
```

Decision rule:
- `<50K rows/side` and interactive latency target: keep in-process.
- `>=50K rows/side`, spill expected, or async requested: route to sidecar.
- Very large async outputs: sidecar writes result to S3 and returns job/result handle.

### 7.4 Memory Accounting Implementation

Go doesn't provide per-goroutine memory tracking. The approach is cooperative accounting:

```go
type MemoryTracker struct {
    limit   int64
    used    atomic.Int64
    spilled atomic.Bool
}

func (m *MemoryTracker) Reserve(bytes int64) bool {
    new := m.used.Add(bytes)
    if new > m.limit {
        m.used.Add(-bytes)
        return false
    }
    return true
}

func (m *MemoryTracker) Release(bytes int64) {
    m.used.Add(-bytes)
}

func estimateRowBytes(rows []Row) int64 {
    if len(rows) == 0 {
        return 0
    }
    // Sample first row, multiply by count
    // Each map entry: ~100 bytes (key string ~30 bytes, value interface{} ~50 bytes, map overhead ~20 bytes)
    sampleSize := int64(len(rows[0])) * 100
    return sampleSize * int64(len(rows))
}
```

This is approximate by design. Exact byte-level tracking would require intercepting every allocation, which Go doesn't support without significant performance overhead. The goal is order-of-magnitude protection, not byte-exact accounting.

---

## 8. Join Execution Engine

### 8.1 Join Types

The system supports **INNER JOIN** only (per the SQL subset). The join condition must be an equality predicate (`ON a.key = b.key`).

### 8.2 Hash Join (Primary Strategy)

The hash join is the default and optimal strategy for equi-joins with bounded result sets:

```
Phase 1 — Build:
  Pick the smaller side (fewer rows). Build a hash map: join_key → []Row.
  O(n) time, O(n) space where n = smaller side.

Phase 2 — Probe:
  Iterate over the larger side. For each row, look up the join key in the hash map.
  For each match, emit a merged row.
  O(m) time where m = larger side.

Total: O(n + m) time, O(min(n, m)) space.
```

**Why smaller side as build**: The hash map needs to fit in memory. By picking the smaller side, we minimize memory consumption.

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

### 8.3 When Hash Join Fails: Spill to DuckDB

If the build side exceeds the memory budget (e.g., 50K+ rows × 100 bytes/row = 5MB+ hash table), the executor switches to DuckDB-backed execution. For sync requests this can remain in-process when small enough; for large/async requests it routes to the sidecar.

```
1. Write left-side rows to /tmp/left.parquet
2. Write right-side rows to /tmp/right.parquet
3. DuckDB executes: SELECT * FROM left.parquet l JOIN right.parquet r ON l.key = r.key
4. DuckDB handles the join using its own disk-backed hash join implementation
5. Result rows read back into Go (if small) or written to S3 (if large)
```

DuckDB's join engine handles:
- Grace hash join (partitions that don't fit in memory are spilled to disk)
- Sort-merge join (fallback for extremely skewed data)
- Nested loop join (only for special cases, not applicable here since we only support equi-joins)

### 8.4 Join Result Caching

The prototype does NOT cache join results — only individual connector fetch results (source cache). This is deliberate:

- Join result caching creates a combinatorial explosion of cache keys (every combination of filter sets across both sources)
- The source cache already eliminates the expensive part (SaaS API calls)
- If both source caches are warm, the join is computed from cached data in <10ms (for typical result sets)

Production materialization cache (Tier 2, documented in data-storage-and-cache-strategy.md) is for the case where:
- Source cache TTLs have expired but the join result is still valid
- The join itself is expensive (100K+ rows per side)
- The same join pattern repeats frequently

---

## 9. Async vs Sync Execution Paths

### 9.1 Why Two Paths

Not all queries can complete within a synchronous HTTP timeout (10-30s). Reasons:

| Scenario | Why it's slow |
|---|---|
| Large result set | Connector needs to paginate through 100 pages of 100 rows each from Jira |
| Rate limit exhausted | Connector can't call the SaaS API until the budget refills (could be minutes) |
| Slow SaaS API | Some APIs (Salesforce SOQL on large objects) take 10-30s for complex queries |
| Large join | Cross-product of two 50K-row sources takes time to process |
| Spill to S3 | Writing result Parquet to S3 adds latency |

### 9.2 Decision Logic

```
Query arrives → parse → plan

Is result estimatable?
  Yes, <10K rows per source, all source caches warm?
    → SYNC path (inline response)

  Yes, >50K rows or join of >10K × >10K?
    → ASYNC path (job queue)

  Unknown (first query, no cache, no stats)?
    → Start SYNC, switch to ASYNC if timeout approaching

Rate limit status?
  All connectors have budget → proceed on chosen path
  Any connector budget exhausted, async overflow enabled → ASYNC
  Any connector budget exhausted, no async overflow → 429 Retry-After
```

### 9.3 Sync Path

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
  │                          │                              │
  │                          │  ◄── QueryResponse           │
  │  ◄── 200 OK + rows      │                              │
  │      + metadata          │                              │
```

The response includes rows inline:
```json
{
  "columns": [...],
  "rows": [...],
  "freshness_ms": 142,
  "cache_hit": true,
  "rate_limit_status": [...],
  "trace_id": "abc123"
}
```

### 9.4 Async Path

```
Client                     Gateway                Job Queue               Async Worker
  │                          │                       │                       │
  │  POST /v1/query          │                       │                       │
  │  allow_async: true       │                       │                       │
  │ ────────────────────►    │                       │                       │
  │                          │── rate limit check    │                       │
  │                          │── budget exhausted    │                       │
  │                          │── overflow=async      │                       │
  │                          │                       │                       │
  │                          │  enqueue(query)       │                       │
  │                          │ ─────────────────────►│                       │
  │                          │                       │                       │
  │  ◄── 202 Accepted       │                       │                       │
  │      job_id: "xyz789"   │                       │                       │
  │      poll_url: /v1/jobs/xyz789                   │                       │
  │                          │                       │                       │
  │                          │                       │── budget refills      │
  │                          │                       │                       │
  │                          │                       │  dequeue(query)       │
  │                          │                       │ ─────────────────────►│
  │                          │                       │                       │── fetch connectors
  │                          │                       │                       │── join + filter
  │                          │                       │                       │── write result to S3
  │                          │                       │                       │
  │                          │                       │  ◄── result_url      │
  │  GET /v1/jobs/xyz789     │                       │                       │
  │ ────────────────────►    │                       │                       │
  │  ◄── 200 OK             │                       │                       │
  │      status: COMPLETED   │                       │                       │
  │      result_url: s3://...│                       │                       │
```

The async response:
```json
{
  "job_id": "xyz789",
  "status": "QUEUED",
  "estimated_completion_ms": 45000,
  "poll_url": "/v1/jobs/xyz789"
}
```

After completion:
```json
{
  "job_id": "xyz789",
  "status": "COMPLETED",
  "result_url": "https://s3.../results/xyz789.parquet",
  "result_format": "parquet",
  "row_count": 150000,
  "freshness_ms": 0,
  "trace_id": "abc123"
}
```

### 9.5 Sync-to-Async Handoff

The most interesting case is when a query starts synchronous but the executor realizes it won't finish in time:

```
Client sends sync request (no allow_async flag)
  → Gateway sets 10s timeout context
  → Executor starts fetching connectors concurrently
  → After 7s, one connector is still paginating
  → Executor checks: 3s remaining, estimated 5s more needed
  → Executor decision:
      If allow_async in request → enqueue remainder, return 202 with job_id
      If !allow_async → wait until timeout → return SOURCE_TIMEOUT with partial results hint
```

This dual-mode execution is handled by monitoring `ctx.Deadline()` in the fetch loop. The executor doesn't commit to sync or async upfront — it starts sync and escalates to async if needed (and permitted by the client).

---

## 10. End-to-End Data Flow (Putting It All Together)

### Complete Request Lifecycle for a JOIN Query

```sql
SELECT gh.title, gh.state, j.issue_key, j.status, j.assignee
FROM github.pull_requests gh
JOIN jira.issues j ON gh.jira_issue_id = j.issue_key
WHERE gh.state = 'open'
LIMIT 10
```

```
Step 1 — Gateway receives request
  ├── Validate JWT → extract Principal{user_id: "alice", tenant_id: "acme", roles: ["developer"]}
  ├── Generate trace_id: "t-abc123"
  ├── L1 rate-limit check: acme tenant global → PASS (80/100 remaining)
  ├── L2 rate-limit check: acme:alice user global → PASS (15/20 remaining)
  └── Set timeout context: 10s

Step 2 — Parser transforms SQL → QueryPlan
  ├── Parse AST → 2 sources (github, jira), 1 JOIN, 1 WHERE predicate
  ├── Classify filters:
  │     gh.state = 'open' → alias "gh" maps to github → PUSHDOWN to github
  │     (no post-fetch filters)
  ├── JOIN spec: inner join, left=gh.jira_issue_id, right=j.issue_key
  └── LIMIT 10 propagated to both SourceQuery structs

Step 3 — Executor starts
  ├── Entitlement pre-check:
  │     CheckTableAccess(alice["developer"], "github.pull_requests") → PASS
  │     CheckTableAccess(alice["developer"], "jira.issues") → PASS
  │
  ├── Launch concurrent fetches via errgroup:
  │
  │   ┌─── Goroutine 1 (GitHub) ───────────────────────────────────────┐
  │   │ L3 rate-limit: reserve token for acme:github → PASS            │
  │   │ L4 rate-limit: reserve token for acme:alice:github → PASS      │
  │   │ Cache lookup: hash(acme + github + pull_requests + {state:open})│
  │   │   → MISS (first query)                                         │
  │   │ Bulkhead: acquire goroutine slot (12/50 in use)                │
  │   │ GitHub.Fetch(ctx, alice, {table: pull_requests,                │
  │   │              filters: [{state = 'open'}], limit: 10})          │
  │   │   → connector calls GitHub API (or returns mock data)          │
  │   │   → simulated latency: 50ms                                    │
  │   │   → returns 100 open PRs (broader than LIMIT because           │
  │   │     cache stores the full filtered result set)                  │
  │   │ Cache write: store 100 rows with TTL=300s                      │
  │   │ Return 100 rows + meta{connector: github, cache_hit: false}    │
  │   └────────────────────────────────────────────────────────────────┘
  │
  │   ┌─── Goroutine 2 (Jira) ────────────────────────────────────────┐
  │   │ L3 rate-limit: reserve token for acme:jira → PASS             │
  │   │ L4 rate-limit: reserve token for acme:alice:jira → PASS       │
  │   │ Cache lookup: hash(acme + jira + issues + {})                  │
  │   │   → MISS (no filters pushed, broad fetch)                      │
  │   │ Bulkhead: acquire goroutine slot (8/30 in use)                 │
  │   │ Jira.Fetch(ctx, alice, {table: issues, filters: [], limit: 10})│
  │   │   → connector calls Jira API (or returns mock data)            │
  │   │   → simulated latency: 80ms                                    │
  │   │   → returns 200 issues (all issues for tenant)                 │
  │   │ Cache write: store 200 rows with TTL=300s                      │
  │   │ Return 200 rows + meta{connector: jira, cache_hit: false}      │
  │   └────────────────────────────────────────────────────────────────┘
  │
  ├── Both goroutines complete. errgroup.Wait() returns nil.
  │
  ├── Post-fetch RLS (per source, before join):
  │     GitHub: developer alice → row_filter: author = alice
  │       100 open PRs → 20 authored by alice
  │     Jira: developer alice → row_filter: assignee = alice
  │       200 issues → 40 assigned to alice
  │
  ├── Post-fetch CLS (per source, before join):
  │     GitHub: email column masked for non-admin roles
  │       20 PRs with email → email = "[REDACTED]"
  │     Jira: no column masks for issues table
  │
  ├── Hash join:
  │     Build side: 20 GitHub PRs (smaller side)
  │       Hash map: {PROJ-100: [row1], PROJ-102: [row3], ...}
  │     Probe side: 40 Jira issues
  │       For each issue, lookup issue_key in hash map
  │       Matches: 15 rows (not all PRs have matching issues)
  │
  ├── Post-filters: none (no cross-table WHERE predicates)
  │
  ├── Projection: select gh.title, gh.state, j.issue_key, j.status, j.assignee
  │     15 joined rows → keep only 5 columns per row
  │
  ├── Ordering: none specified
  │
  └── Limit: 10 → return first 10 of 15 rows

Step 4 — Response
  {
    "columns": ["gh.title", "gh.state", "j.issue_key", "j.status", "j.assignee"],
    "rows": [... 10 rows ...],
    "freshness_ms": 0,
    "cache_hit": false,
    "rate_limit_status": [
      {"connector_id": "github", "allowed": true},
      {"connector_id": "jira", "allowed": true}
    ],
    "sources": [
      {"connector_id": "github", "table": "github.pull_requests", "rows_scanned": 100},
      {"connector_id": "jira", "table": "jira.issues", "rows_scanned": 200}
    ],
    "trace_id": "t-abc123"
  }
```

### Second Query (Same User, 30 Seconds Later)

```
Same SQL, same user, same tenant.

Step 2 — Parser: same QueryPlan (deterministic)

Step 3 — Executor:
  ├── Entitlement pre-check: PASS (same roles)
  ├── Goroutine 1 (GitHub):
  │     L3/L4 rate-limit: RESERVE
  │     Cache lookup: hash(acme + github + pull_requests + {state:open})
  │       → HIT (30s old, within default max_staleness of 300s)
  │     Rate-limit reservation: CANCEL (no API call needed)
  │     Return 100 cached rows + meta{cache_hit: true, freshness_ms: 30000}
  │
  ├── Goroutine 2 (Jira):
  │     L3/L4 rate-limit: RESERVE
  │     Cache lookup: hash(acme + jira + issues + {})
  │       → HIT (30s old)
  │     Rate-limit reservation: CANCEL
  │     Return 200 cached rows + meta{cache_hit: true, freshness_ms: 30000}
  │
  ├── RLS/CLS applied on cached data (same as before)
  ├── Hash join on cached data (same result)
  ├── Project + limit
  │
  └── Response: same rows, but:
        freshness_ms: 30000 (data is 30 seconds old)
        cache_hit: true
        GitHub API calls: 0
        Jira API calls: 0
```

**Result**: Two queries, two users, one SaaS API call each. Every subsequent query from any user in the same tenant for the next 5 minutes hits cache.

---

## 11. Prototype vs Production Gap

| Aspect | Prototype (Current) | Production Design |
|---|---|---|
| **Connector deployment** | In-process, no isolation | In-process with bulkhead (goroutine pool, circuit breaker, memory budget, panic recovery) |
| **Connector interface** | Batch `Fetch()` returning `[]Row` | Streaming `FetchStream()` returning `<-chan RowBatch` for large result sets |
| **Parser** | vitess-sqlparser (MySQL) | ANTLR4 custom grammar with schema-aware validation |
| **Filter classification** | Push all single-table predicates | Capability-aware: only push what connector declares it supports |
| **Join engine** | In-memory hash join only | Hash join + DuckDB spill for large result sets |
| **Memory management** | Unbounded (relies on Go GC) | Per-connector memory budgets, `GOMEMLIMIT`, spill-to-disk trigger |
| **Spill strategy** | Not implemented | Parquet files on local SSD → DuckDB execution → S3 for final result |
| **Async path** | Not implemented | Job queue (SQS/Redis Stream) for rate-limited and large queries |
| **Result delivery** | Always inline JSON | Inline for small results, S3 presigned URL for large results |
| **Concurrency control** | `errgroup` per query | `errgroup` per query + bounded goroutine pool per connector |
| **Fault isolation** | None (panic = process crash) | `recover()` + circuit breaker + per-connector timeout |
| **Capability model** | Implicit (all connectors push all filters) | Explicit YAML declaration per connector, planner consults at plan time |
| **Cost estimation** | None | Catalog-driven cardinality estimates for join ordering and pushdown decisions |

---

## 12. Goroutine Pool Provisioning & Resource Sizing

### 12.1 Critical Mental Model: Goroutines Are Not Threads

This is the most important clarification. In Go, **goroutines are not OS threads**. A goroutine costs ~2KB of stack memory. 10,000 goroutines can run on 8 OS threads. The Go runtime multiplexes goroutines onto OS threads via its M:N scheduler (`GOMAXPROCS` controls how many OS threads are active simultaneously, defaults to number of CPU cores).

When we say "goroutine pool per connector," we are NOT allocating CPU threads. We are limiting **how many concurrent in-flight API calls** we make to that connector. This is a concurrency control mechanism, not a CPU allocation.

```
Traditional thread model (Java, C++):
  10 connectors × 50 threads each = 500 OS threads
  8-core machine → 500 threads fighting for 8 cores
  Context switching overhead: ~1-10μs per switch
  Memory: 500 × 1MB stack = 500MB just for stacks

Go goroutine model:
  10 connectors × 50 goroutines each = 500 goroutines
  8-core machine → 8 OS threads (GOMAXPROCS=8)
  Goroutines multiplexed onto 8 threads by Go runtime
  Goroutine switch: ~100ns (10-100x cheaper than thread switch)
  Memory: 500 × 2KB stack = 1MB for stacks
```

**Implication**: Provisioning 500 goroutines costs almost nothing. The goroutine pool per connector limits concurrency toward the SaaS API, not CPU consumption. An "idle" goroutine pool with 50 slots but 0 active goroutines consumes exactly the memory of the channel buffer (50 × 8 bytes = 400 bytes). Negligible.

### 12.2 What Drives Goroutine Pool Size Per Connector

The pool size is driven by the **SaaS API's concurrency constraint**, not by CPU cores:

| Connector | What constrains concurrency | Pool size |
|---|---|---|
| **GitHub** | 5,000 req/hour budget. Burst OK. But if we send 100 concurrent requests, each takes 200ms, we burn 100 tokens in 200ms. Budget-based cap. | 20-30 |
| **Jira Cloud** | Hard concurrency limit: 10 simultaneous in-flight requests per tenant. Exceeding → immediate 429. | 10 |
| **Salesforce** | 25 concurrent requests per 15s window. | 25 |
| **Notion** | 3 req/sec, strict. No concurrency benefit beyond 3-5. | 5 |
| **Google Workspace** | Per-user-per-100s rolling window. | 15-20 |

The formula:

```
goroutine_pool_size = min(
    connector.max_concurrent_requests,      // SaaS API hard limit
    connector.rate_per_second × avg_latency_seconds × 1.5,  // steady-state concurrency
    global_max_per_connector,               // our internal cap (e.g., 50)
)
```

Example for GitHub:
```
max_concurrent_requests = unlimited (budget-based)
rate_per_second = 5000/3600 ≈ 1.4
avg_latency = 0.2s
steady_state_concurrency = 1.4 × 0.2 × 1.5 ≈ 0.42

→ In steady state, we only need ~1 goroutine active at a time for GitHub!
→ Pool size of 20 handles burst (e.g., 20 users simultaneously query GitHub)
→ Pool size of 50 is generous overkill
```

Example for Jira:
```
max_concurrent_requests = 10 (hard limit from Jira)
→ Pool size = 10. Period. Exceeding this gets us 429'd.
```

### 12.3 Pod Sizing: CPU, Memory, and Connector Density

**CPU**: Connector calls are **I/O-bound** (waiting for SaaS API HTTP responses). While a goroutine is waiting on `net.Read()`, it does NOT consume a CPU core. The Go runtime parks it and schedules another goroutine.

Actual CPU work per query:
```
SQL parsing:          ~100μs
Filter classification: ~10μs
Rate-limit check:     ~1μs
Cache lookup:         ~5μs (in-memory hash)
JSON unmarshal:       ~1-5ms per 1000 rows (from SaaS API response)
Hash join:            ~0.5ms per 1000 rows
RLS/CLS filtering:    ~0.1ms per 1000 rows
JSON marshal:         ~1-5ms per 1000 rows (for HTTP response)
──────────────────────────────────────────────
Total CPU per query:  ~5-15ms
```

At 1,000 QPS with 10ms CPU per query:
```
1000 × 10ms = 10,000ms = 10 CPU-seconds per wall-clock second = 10 cores
```

So a 1k QPS pod needs roughly 10-12 cores (with headroom for GC, scheduling). But this is **query QPS**, not connector concurrency. Each query touches 1-2 connectors, and the connector call is I/O wait, not CPU.

**Memory**: The binding constraint. Each concurrent query holds rows in memory:
```
Per query: ~1000 rows × ~500 bytes/row × 2 sources = ~1MB
100 concurrent queries in-flight: ~100MB
Cache: 500MB-1GB (configurable)
Go runtime + overhead: ~300MB
──────────────────────────────────────────────
Total: ~1.5-2GB per pod for ~100 concurrent queries
```

**Connector density**: How many connectors can one pod host?

```
If all 10 connectors run at full concurrency simultaneously:
  10 connectors × 30 goroutines × 1000 rows × 500 bytes = 150MB peak
  This is fine. 150MB for in-flight connector data on a 4GB pod.

If only 3 connectors are active at any moment (typical):
  3 × 30 × 1000 × 500 = 45MB peak
  Way under budget.
```

**The idle-time question**: You're right that not all connectors are active simultaneously. But since goroutine pools cost almost nothing when idle (just the channel buffer), there's no meaningful waste. The "idle" goroutine pool for an unused connector is literally 400 bytes. We can afford to provision all 10 connectors per pod without any overprovisioning concern.

### 12.4 Two-Tier Pool Architecture: Global + Per-Connector

The correct architecture is not "fixed pool per connector" alone. It's a **shared global pool with per-connector caps**:

```
┌──────────────────────────────────────────────────────────────────┐
│  Pod Global Concurrency Pool                                      │
│  Max: 200 concurrent connector fetches across ALL connectors     │
│                                                                  │
│  ┌──────────────────────────────────────────────────────────────┐│
│  │  Per-Connector Caps (subset of global pool)                  ││
│  │                                                              ││
│  │  GitHub:      max 30   ─┐                                    ││
│  │  Jira:        max 10   ─┤                                    ││
│  │  Salesforce:  max 25   ─┤── sum of caps can exceed 200       ││
│  │  Notion:      max 5    ─┤   because they won't all be at     ││
│  │  Slack:       max 20   ─┤   max simultaneously               ││
│  │  Google:      max 20   ─┤                                    ││
│  │  Zendesk:     max 15   ─┤   (statistical multiplexing)       ││
│  │  HubSpot:     max 15   ─┤                                    ││
│  │  ...          ...      ─┘                                    ││
│  │  Sum of caps: 140+                                           ││
│  │  Global cap:  200 (hard ceiling)                             ││
│  └──────────────────────────────────────────────────────────────┘│
└──────────────────────────────────────────────────────────────────┘
```

**How it works**:

```go
type ConnectorPoolManager struct {
    globalSem       chan struct{}            // global concurrency cap
    perConnectorSem map[string]chan struct{} // per-connector caps
}

func (m *ConnectorPoolManager) Acquire(ctx context.Context, connectorID string) error {
    // Check per-connector cap first (fast rejection)
    connSem, ok := m.perConnectorSem[connectorID]
    if !ok {
        return fmt.Errorf("unknown connector: %s", connectorID)
    }

    select {
    case connSem <- struct{}{}:
        // Got per-connector slot
    case <-ctx.Done():
        return ctx.Err()
    }

    // Now acquire global slot
    select {
    case m.globalSem <- struct{}{}:
        // Got global slot
        return nil
    case <-ctx.Done():
        // Timed out waiting for global slot — release per-connector slot
        <-connSem
        return ctx.Err()
    }
}

func (m *ConnectorPoolManager) Release(connectorID string) {
    <-m.globalSem
    <-m.perConnectorSem[connectorID]
}
```

**Why two tiers**:
- **Per-connector cap** protects the SaaS API (GitHub allows 30 concurrent, Jira allows 10)
- **Global cap** protects the pod (no more than 200 total concurrent fetches, regardless of connector mix)

**Statistical multiplexing** (what you called "not more than 8 connectors used at a time"):

The sum of per-connector caps (140+) exceeds the global cap (200). This is intentional — it's the same principle as airline overbooking or network bandwidth sharing. Not all connectors run at full concurrency simultaneously. The global cap catches the case where they unexpectedly do.

If a burst hits multiple connectors simultaneously and the global pool is full, excess requests block (with timeout). The per-connector cap ensures that one runaway connector can't consume the entire global pool.

### 12.5 Configuration: How Connector Pool Sizes Are Set

Pool sizes live in the control plane config, not hardcoded:

```yaml
# configs/connector-pools.yaml (loaded from control plane, cached 30s)
global:
  max_concurrent_fetches: 200      # per pod
  max_memory_bytes: 1610612736     # 1.5GB for all connectors combined

connectors:
  github:
    max_concurrent: 30
    max_memory_bytes: 314572800    # 300MB
    fetch_timeout: 5s
    circuit_breaker:
      failure_threshold: 5
      cooldown: 30s

  jira:
    max_concurrent: 10             # hard limit from Jira Cloud API
    max_memory_bytes: 314572800    # 300MB
    fetch_timeout: 8s              # JQL can be slow
    circuit_breaker:
      failure_threshold: 5
      cooldown: 30s

  salesforce:
    max_concurrent: 25
    max_memory_bytes: 524288000    # 500MB (Salesforce returns larger payloads)
    fetch_timeout: 15s             # SOQL on large objects is slow
    circuit_breaker:
      failure_threshold: 3         # lower threshold — SF errors are expensive
      cooldown: 60s

  notion:
    max_concurrent: 5              # 3 req/s hard limit, 5 gives minimal concurrency
    max_memory_bytes: 104857600    # 100MB (Notion returns small payloads)
    fetch_timeout: 5s
    circuit_breaker:
      failure_threshold: 5
      cooldown: 30s
```

**Per-tenant overrides** (stored in tenant config, loaded from control plane):

```yaml
tenants:
  acme-corp:
    connectors:
      github:
        max_concurrent: 50         # GitHub Enterprise plan → higher limits
        max_memory_bytes: 524288000
```

The pool manager reads this config at startup and refreshes it periodically (30s TTL). Config changes don't require pod restart — the pool sizes are updated dynamically.

### 12.6 I/O-Heavy vs CPU-Heavy Connectors

Different connectors have different resource profiles:

| Connector | I/O Profile | CPU Profile | Resource Strategy |
|---|---|---|---|
| **GitHub** | Fast API (50-200ms), small payloads (JSON, ~1KB/row) | Low CPU (small response parsing) | Standard pool, no special treatment |
| **Jira** | Medium API (200-800ms), medium payloads (rich issue objects, ~5KB/row) | Medium CPU (JQL response parsing, nested fields) | Larger timeout, lower concurrency |
| **Salesforce** | Slow API (500ms-5s for SOQL), large payloads (10-50KB/row for full objects) | High CPU (complex nested JSON, relationship fields) | Larger timeout, larger memory budget, lower concurrency |
| **Google Drive** | Fast list API, slow content API | Low for metadata, high for content extraction | Two-phase: metadata fast path, content lazy-load |
| **Notion** | Slow API (300ms-1s), small payloads | Low CPU | Very low concurrency (API-limited), standard memory |

**The key insight**: We don't need different thread pool *types* for I/O-heavy vs CPU-heavy connectors. Go's goroutine scheduler handles this automatically:

- **I/O-heavy connector** (GitHub): goroutine spends 95% of its time blocked on `net.Read()`. Go parks it, schedules another goroutine. Even 30 concurrent GitHub goroutines consume <1% CPU.
- **CPU-heavy connector** (Salesforce response parsing): goroutine spends more time on CPU (JSON parsing). Go schedules it on a real OS thread. But this is still a few ms per response.

The differentiation is in the pool config (concurrency, timeout, memory budget), not in the pool implementation. One `ConnectorPoolManager` implementation serves all connector types.

---

## 13. Memory Accounting — Practical Implementation

### 13.1 Why This Is Hard (and Why Approximate Is Fine)

Go does not support per-goroutine or per-request memory limits. The runtime allocates memory from a global heap. There's no `setrlimit(RLIMIT_AS)` equivalent for goroutines.

**What we CAN'T do**: "Allocate exactly 300MB to the GitHub connector and guarantee it never exceeds that."

**What we CAN do**: Track how much data the connector returned, reject results that would exceed the budget, and rely on process-level limits (`GOMEMLIMIT`, k8s pod limits) as the hard ceiling.

### 13.2 Where Memory Is Actually Consumed

```
A single connector fetch lifecycle:

1. HTTP response body arrives from SaaS API (network buffer)    ~50KB-5MB
     ↓ (json.Unmarshal)
2. Parsed into internal structs/maps (Go heap)                  ~2x of JSON size
     ↓ (connector returns []Row)
3. []Row held in executor (Go heap)                             same as #2
     ↓ (hash join)
4. Hash map + joined rows (Go heap)                             ~1.5x of input
     ↓ (projection + marshaling)
5. JSON response body (output buffer)                           ~0.5x of result rows
```

Peak memory for one fetch = roughly **3-4x the raw JSON response size**. This is where the budget enforcement matters.

### 13.3 Three-Layer Memory Protection

```
Layer 1: Connector-level response size limit (preventive)
  ├── Enforced INSIDE the connector, during pagination
  ├── "Stop fetching pages after accumulating 50,000 rows or 50MB of data"
  ├── The connector controls this, not the SaaS API
  └── Independent of whether the SaaS API supports a size limit header

Layer 2: Bulkhead memory accounting (detective)
  ├── After Fetch() returns, estimate the row payload size
  ├── If cumulative connector memory exceeds budget → reject result
  └── Concurrent requests from the same connector share the budget

Layer 3: Process-level hard limits (protective)
  ├── GOMEMLIMIT (Go runtime soft limit → aggressive GC)
  ├── k8s pod memory limit (hard limit → OOMKill → restart)
  └── These are the backstop when estimates are wrong
```

### 13.4 Layer 1: Connector-Side Pagination Cutoff

This is the answer to "what if the SaaS API doesn't support data limit headers": **you don't need the API to support it.** The connector controls pagination. Every SaaS API is paginated — the connector chooses how many pages to fetch.

```go
func (c *JiraConnector) Fetch(ctx context.Context, principal *Principal, sq SourceQuery) ([]Row, SourceMeta, error) {
    var allRows []Row
    var cursor string
    accumulatedBytes := int64(0)

    for {
        page, nextCursor, err := c.fetchPage(ctx, sq, cursor)
        if err != nil {
            return nil, SourceMeta{}, err
        }

        for _, row := range page {
            allRows = append(allRows, row)
            accumulatedBytes += estimateSingleRowBytes(row)

            // Hard cutoff: stop fetching pages if we've accumulated too much data.
            // This protects the process regardless of what the SaaS API returns.
            if accumulatedBytes > c.maxFetchBytes {
                return allRows, SourceMeta{
                    RowsScanned: len(allRows),
                    Truncated:   true,
                    TruncateReason: fmt.Sprintf(
                        "response truncated at %dMB (limit: %dMB)",
                        accumulatedBytes/(1024*1024),
                        c.maxFetchBytes/(1024*1024),
                    ),
                }, nil
            }

            // LIMIT enforcement: stop early if we have enough rows
            if sq.Limit > 0 && len(allRows) >= sq.Limit {
                return allRows, SourceMeta{RowsScanned: len(allRows)}, nil
            }
        }

        if nextCursor == "" {
            break
        }
        cursor = nextCursor
    }

    return allRows, SourceMeta{RowsScanned: len(allRows)}, nil
}
```

**Key**: The connector decides when to stop paginating. It doesn't need to ask the SaaS API "please return at most 50MB." It fetches page by page and stops when its local accumulator exceeds the budget. The SaaS API doesn't need to support any special headers.

The `maxFetchBytes` comes from the connector config (e.g., 50MB for Jira, 100MB for Salesforce). This is the primary memory protection mechanism.

### 13.5 Layer 2: Bulkhead-Level Accounting

After `Fetch()` returns, the bulkhead estimates the memory footprint and tracks it against the per-connector budget:

```go
func estimateRowBytes(rows []Row) int64 {
    if len(rows) == 0 {
        return 0
    }

    // Sample up to 10 rows to get average row size
    sampleSize := min(len(rows), 10)
    totalSample := int64(0)
    for i := 0; i < sampleSize; i++ {
        for k, v := range rows[i] {
            totalSample += int64(len(k))       // map key
            totalSample += estimateValueBytes(v) // map value
            totalSample += 80                    // map entry overhead (Go runtime)
        }
    }
    avgRowBytes := totalSample / int64(sampleSize)

    // Multiply by total count, add slice overhead
    return avgRowBytes*int64(len(rows)) + int64(len(rows))*24 // 24 = slice element pointer
}

func estimateValueBytes(v interface{}) int64 {
    switch val := v.(type) {
    case string:
        return int64(len(val)) + 16 // string header + data
    case int, int64, float64:
        return 8
    case bool:
        return 1
    default:
        return 64 // conservative estimate for complex types
    }
}
```

The tracking happens in the bulkhead:

```go
type MemoryAccountant struct {
    limit int64         // per-connector budget (e.g., 300MB)
    used  atomic.Int64  // current usage across all in-flight fetches
}

func (m *MemoryAccountant) Reserve(bytes int64) (release func(), err error) {
    newUsed := m.used.Add(bytes)
    if newUsed > m.limit {
        m.used.Add(-bytes) // rollback
        return nil, fmt.Errorf("connector memory budget exceeded: %dMB used, %dMB limit",
            newUsed/(1024*1024), m.limit/(1024*1024))
    }
    return func() { m.used.Add(-bytes) }, nil
}
```

Usage in the bulkhead:

```go
rows, meta, err := b.connector.Fetch(fetchCtx, principal, sq)
if err != nil {
    return nil, meta, err
}

rowBytes := estimateRowBytes(rows)
release, err := b.memory.Reserve(rowBytes)
if err != nil {
    // Connector returned too much data for our budget.
    // We have the data in memory momentarily, but we reject it
    // and let GC reclaim it.
    return nil, meta, qerrors.New(qerrors.CodeSourceTimeout,
        fmt.Sprintf("connector %s: %v", b.connector.ID(), err),
        b.connector.ID(), 0, nil)
}

// Caller must call release() when done with the rows.
// In practice, the executor calls this after join/project/response.
```

### 13.6 Why Approximate Accounting Works

The estimate can be 20-30% off from actual heap usage (Go map overhead, GC metadata, string interning). That's fine because:

1. **Layer 1 (connector-side pagination cutoff)** already prevents truly huge responses
2. **Layer 2 (bulkhead accounting)** catches the next tier — "300MB budget, and this 350MB response is too big"
3. **Layer 3 (GOMEMLIMIT + OOMKill)** catches everything else

The goal is **order-of-magnitude protection**, not byte-exact accounting. If the estimate says 280MB and reality is 340MB, we're still within the pod's memory headroom. If the estimate says 280MB and reality is 2.8GB, that's a 10x error in the estimate — which would mean the estimator is fundamentally broken, and GOMEMLIMIT + OOMKill save us.

### 13.7 Memory Lifecycle for a JOIN Query

```
Time    Memory Event                                      Cumulative Usage
─────── ───────────────────────────────────────────────── ─────────────
t=0     Query arrives                                     0 MB
t=1     GitHub Fetch returns 5,000 rows (~5MB)            5 MB
          → bulkhead reserves 5MB from GitHub budget
t=2     Jira Fetch returns 10,000 rows (~12MB)            17 MB
          → bulkhead reserves 12MB from Jira budget
t=3     RLS filters GitHub rows: 5,000 → 1,000            17 MB (no release yet)
t=4     RLS filters Jira rows: 10,000 → 2,000             17 MB
t=5     Hash join: build on 1,000 GitHub rows (~1MB map)   18 MB
t=6     Hash join: probe 2,000 Jira rows → 800 matches     19 MB (joined rows)
t=7     Drop pre-join GitHub rows (GC-eligible)            14 MB
          → release 5MB from GitHub memory budget
t=8     Drop pre-join Jira rows (GC-eligible)              2 MB
          → release 12MB from Jira memory budget
t=9     Project + limit → 10 rows (~10KB)                  ~0 MB
t=10    Marshal response, send to client                   0 MB
          → release joined rows
```

Peak memory for this query: ~19MB. On a pod with a 1.5GB connector memory budget, we can handle ~80 such queries concurrently. At 1k QPS with average query latency of 100ms (after cache hits), we have ~100 concurrent queries — within budget.

---

## 14. Entitlement Service

### 14.1 Policy DSL

Policies are expressed in YAML, version-controlled alongside the code, and loaded at startup with a 30-60s TTL refresh from the control plane. Each table entry has three sections:

```yaml
tables:
  github.pull_requests:
    allowed_roles:           # who can query this table at all
      - admin
      - developer
      - viewer

    row_filters:             # RLS: injected WHERE clause per role
      - role: developer
        column: author
        principal_field: username  # row.author must equal caller's username

    column_masks:            # CLS: mask column values for non-privileged roles
      email:
        except_roles: [admin]
        mask: "[REDACTED]"
```

This is a static policy DSL — simple, auditable, and version-controlled. In production it evolves toward a dynamic store (OPA + OPAL) that supports tenant-specific overrides, time-bound rules, and programmatic policy generation.

### 14.2 Execution Pipeline

Entitlements are applied at two points, never just one:

```
Step 1 — Pre-fetch: CheckTableAccess(principal, table)
  Called before any connector fetch.
  If the user's roles don't appear in allowed_roles for any queried table
  → return 403 ENTITLEMENT_DENIED immediately.
  No API call is made. No rate-limit budget consumed.

Step 2 — Post-fetch: ApplyRLS then ApplyCLS
  Called after connector fetch, before join.
  ApplyRLS: filter rows where row[column] != principal[field]
  ApplyCLS: replace restricted column values with mask string
  RLS runs before CLS — no point masking columns on rows about to be dropped.
```

The critical design decision: RLS and CLS are applied to **cached data, not at fetch time**. The cache is tenant-scoped, not user-scoped. One API call serves all users of a tenant; each user's security view is derived locally by applying their RLS/CLS filters to the shared cached dataset. This is the same model as Postgres row-level security.

Production extension: for large tables, the planner injects RLS predicates (e.g. `assignee = 'alice'`) directly into `SourceQuery.Filters` before pushdown, trading cache reuse for narrower fetches.

### 14.3 Principal: Roles vs Scopes

The `Principal` struct carries two distinct auth concepts:

```go
type Principal struct {
    UserID   string
    TenantID string
    Roles    []string  // coarse-grained: admin, developer, viewer
    Scopes   []string  // fine-grained OAuth: github:read, jira:issues:read
}
```

**Roles** (coarse-grained) drive the policy DSL — `allowed_roles`, `row_filters`. These come from the OIDC `id_token` claims issued by the enterprise IdP (Okta, Azure AD, Google Workspace).

**Scopes** (fine-grained) drive connector-level access — a user with role `developer` but missing scope `jira:issues:read` is denied at the connector level, not the policy level. These come from the OAuth access token issued by the source SaaS app.

This mirrors real enterprise SSO: the IdP issues identity (roles), the SaaS app issues delegated access (scopes). Both are enforced; neither is sufficient alone.

---

## Summary of Key Decisions

| Decision | Rationale |
|---|---|
| **In-process connectors with bulkhead isolation** | Avoids serialization overhead on the data path. In-process is the industry standard at this scale (Trino, Presto, Calcite, Dremio). Bulkhead provides fault isolation without the network cost. |
| **Executor as the conductor** | Single component owns all orchestration (cache, rate-limit, entitlements, join, spill). Simpler than distributed coordination between executor and connectors. |
| **Planner as pure function** | No I/O, no side effects. Produces a QueryPlan struct that the executor consumes. Enables planner replacement (cost-based, ML-assisted) without touching the executor. |
| **Hash join by default, hybrid DuckDB spill path** | Hash join is O(n+m) and optimal for equi-joins under 50K rows per side. Use in-process DuckDB for latency-sensitive small spill cases; use sidecar DuckDB for async/large spill cases to isolate memory and protect sync traffic. |
| **Streaming interface for production connectors** | Bounds memory: executor processes row batches as they arrive, doesn't hold entire result set. Enables pipeline parallelism and early termination on LIMIT. |
| **Memory accounting is approximate** | Go doesn't support per-goroutine memory limits. Approximate accounting (row count × estimated row size) is sufficient to prevent catastrophic OOM. Exact tracking would require instrumenting every allocation — too costly. |
| **Async path is opt-in** | Not all queries benefit from async. Interactive queries should fail fast (429 with Retry-After). Batch/reporting queries should queue gracefully. Client declares intent via `allow_async`. |
| **Connector panic recovery** | A single `recover()` in the bulkhead prevents one connector's crash from taking down the entire gateway process. This is defense-in-depth alongside circuit breakers and timeouts. |
