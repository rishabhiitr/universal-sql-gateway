# Connector

## Key Architectural Decision: Deployment Model

The most consequential design choice for connectors is where they run relative to the query executor.

| Dimension | In-Process (embedded in gateway) | Out-of-Process (separate microservice) |
|---|---|---|
| **Data transfer** | Zero-copy — rows are native Go structs in shared heap | Every row serializes to protobuf, crosses network, deserializes |
| **Join performance** | Optimal — executor and connector share memory | Network transfer dominates join cost for large result sets |
| **Fault isolation** | A panicking connector can crash the gateway process | A crashing connector pod doesn't affect gateway or other connectors |
| **Memory isolation** | Shared GOMEMLIMIT — one connector can starve others | Per-pod k8s resource limits — OOMKill is contained |
| **Independent scaling** | Cannot scale one connector without scaling all | Scale Jira pods independently of GitHub pods |
| **Independent deployment** | Connector fix requires gateway redeploy | Ship connector fix without touching the gateway |
| **Operational complexity** | One binary, one pod, one pprof endpoint | N additional deployments, scaling configs, health checks |
| **Language** | Must be Go | Any language — useful if vendor provides a Python/Java SDK |

### Decision: In-Process with Bulkhead Isolation

We run connectors in-process. The core reason: in a federated query engine, the process that calls the connector also joins the results. Separating them means serializing the full result set, sending it over the network, and deserializing it — only to then join it. For a join of 10K × 10K rows, the network transfer dominates the join cost.

The in-process cons (panic propagation, memory contention, no independent scaling) are real but addressable without paying the serialization tax. We address them with bulkhead isolation patterns described in the next section.

**When to reconsider**: Move a connector out-of-process only when one of these triggers applies:

| Trigger | Example |
|---|---|
| Connector needs a different language runtime | Vendor provides only a Python/Java SDK (SAP, Oracle) |
| Connector has known memory-safety issues | Untrusted community connectors |
| Connector scales wildly differently from others | A data-lake connector with 100× the traffic of API connectors |
| Regulatory isolation requirement | PCI-DSS, HIPAA connectors that must run in isolated compute |

Even then, out-of-process connectors communicate via gRPC and stream rows (not batch-send), which amortizes serialization cost.

---

## Connector Interface

Every connector implements a 4-method interface. The executor treats connectors as opaque — it never reaches into connector internals.

```go
type Connector interface {
    ID() string
    Tables() []string
    Schema(table string) ([]models.Column, error)
    Fetch(ctx context.Context, principal *models.Principal, sq models.SourceQuery) ([]models.Row, models.SourceMeta, error)
}
```

A new connector registers itself in the registry. The bulkhead wraps it automatically — no changes to the executor, planner, gateway, cache, or rate-limiter:

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
```

---

## Connector Runtime and Isolation

### Concurrency Model

Connector concurrency is constrained by SaaS API limits, not CPU cores.

| Connector | What constrains concurrency | Pool size |
|---|---|---|
| GitHub | Budget-driven (5,000 req/hour) | 20–30 |
| Jira Cloud | Hard in-flight limit | 10 |
| Salesforce | Concurrency window limits | 25 |
| Notion | Strict req/sec limit | 5 |
| Google Workspace | Rolling per-user windows | 15–20 |

Sizing formula:

```
goroutine_pool_size = min(
    connector.max_concurrent_requests,
    connector.rate_per_second * avg_latency_seconds * 1.5,
    global_max_per_connector,
)
```

### Two-Tier Pool Architecture

Use a shared global pool with per-connector caps:

- Per-connector cap protects the upstream SaaS API.
- Global cap protects pod-wide saturation.

```go
type ConnectorPoolManager struct {
    globalSem       chan struct{}
    perConnectorSem map[string]chan struct{}
}

func (m *ConnectorPoolManager) Acquire(ctx context.Context, connectorID string) error {
    connSem, ok := m.perConnectorSem[connectorID]
    if !ok {
        return fmt.Errorf("unknown connector: %s", connectorID)
    }

    select {
    case connSem <- struct{}{}:
    case <-ctx.Done():
        return ctx.Err()
    }

    select {
    case m.globalSem <- struct{}{}:
        return nil
    case <-ctx.Done():
        <-connSem
        return ctx.Err()
    }
}

func (m *ConnectorPoolManager) Release(connectorID string) {
    <-m.globalSem
    <-m.perConnectorSem[connectorID]
}
```

### Bulkhead Isolation

Each connector gets a `ConnectorBulkhead` that wraps every `Fetch()` call with four protection layers:

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

func (b *ConnectorBulkhead) Fetch(ctx context.Context, principal *models.Principal, sq models.SourceQuery) (rows []models.Row, meta models.SourceMeta, err error) {
    // Layer 4: Panic recovery — a panicking connector must not crash the process
    defer func() {
        if r := recover(); r != nil {
            err = qerrors.New(qerrors.CodeSourceTimeout,
                fmt.Sprintf("connector %s panicked: %v", b.connector.ID(), r),
                b.connector.ID(), 0, nil)
            b.breaker.RecordFailure()
            b.metrics.PanicsTotal.Inc()
        }
    }()

    // Layer 1: Circuit breaker
    if !b.breaker.Allow() {
        return nil, models.SourceMeta{}, qerrors.New(qerrors.CodeSourceTimeout,
            fmt.Sprintf("connector %s circuit open", b.connector.ID()),
            b.connector.ID(), 30, nil)
    }

    // Layer 2: Goroutine pool admission (bounded concurrency)
    select {
    case b.sem <- struct{}{}:
        defer func() { <-b.sem }()
    case <-ctx.Done():
        return nil, models.SourceMeta{}, ctx.Err()
    }

    // Layer 3: Per-connector timeout (inner bound, within query timeout)
    fetchCtx, cancel := context.WithTimeout(ctx, b.timeout)
    defer cancel()

    rows, meta, err = b.connector.Fetch(fetchCtx, principal, sq)
    if err != nil {
        b.breaker.RecordFailure()
        return nil, meta, err
    }
    b.breaker.RecordSuccess()

    // Layer 3 (cont): Memory accounting
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

#### Layer 1: Circuit Breaker per Connector

If a connector fails 5 times in 60 seconds, the circuit opens. Subsequent requests immediately return an error for 30 seconds without calling the connector. This prevents:
- Burning rate-limit budget on requests that will fail anyway
- Timeout cascades that slow down healthy connectors
- Repeated failure traces filling error logs

```
CLOSED (normal) ─── 5 failures in 60s ───► OPEN (reject all)
     ▲                                            │
     │                                     30s cooldown
     │                                            │
     └─── success ◄── HALF-OPEN (allow 1 probe) ◄┘
```

#### Layer 2: Goroutine Pool per Connector

Each connector gets a bounded goroutine pool (a semaphore). This prevents one connector from spawning unlimited goroutines (e.g., 10,000 concurrent paginated fetches) and starving the Go scheduler.

#### Layer 3: Per-Connector Timeout

Each connector gets its own timeout, independent of the query-level timeout. The query-level timeout is the outer bound; the connector timeout is the inner bound:

```
Query timeout: 10s
├── GitHub connector timeout: 5s (fast API, shouldn't need more)
├── Jira connector timeout: 8s (JQL can be slow)
└── Reserved for join + post-processing: 2s minimum
```

If GitHub times out at 5s, the executor still has 5s to get Jira results and return a partial result.

#### Layer 4: Panic Recovery

Go's `recover()` only works in the same goroutine that panicked. Each connector runs in its own goroutine (via `errgroup`), so we wrap `Fetch()` with a deferred `recover()` in the bulkhead. This ensures:

- A nil-pointer dereference inside the GitHub connector returns a `SOURCE_TIMEOUT` error to the executor
- The executor handles it like any other connector error
- The process continues serving other queries

Without this, a single panicking connector takes down the pod, drops all in-flight queries, and triggers a k8s restart.

### Memory Protection: Three Layers

1. **Connector-side pagination cutoff** (preventive): connector stops the page loop when its local accumulator exceeds `maxFetchBytes`.
2. **Bulkhead memory accounting** (detective): after `Fetch()` returns, estimate row bytes and enforce the per-connector budget.
3. **Process-level backstop**: `GOMEMLIMIT` and pod memory limit — if all else fails, the Go GC runs aggressively; if that's not enough, OOMKiller takes the pod.

**Layer 1 — Pagination cutoff in connector:**

```go
for {
    page, nextCursor, err := c.fetchPage(ctx, sq, cursor)
    if err != nil {
        return nil, SourceMeta{}, err
    }

    for _, row := range page {
        allRows = append(allRows, row)
        accumulatedBytes += estimateSingleRowBytes(row)
        if accumulatedBytes > c.maxFetchBytes {
            return allRows, SourceMeta{
                RowsScanned: len(allRows),
                Truncated:   true,
            }, nil
        }
    }

    if nextCursor == "" {
        break
    }
    cursor = nextCursor
}
```

**Layer 2 — Bulkhead memory accounting:**

```go
rowBytes := estimateRowBytes(rows)
release, err := b.memory.Reserve(rowBytes)
if err != nil {
    return nil, meta, qerrors.New(qerrors.CodeSourceTimeout,
        fmt.Sprintf("connector %s: %v", b.connector.ID(), err),
        b.connector.ID(), 0, nil)
}
defer release()
```

Approximate accounting is intentional — the goal is preventing catastrophic overuse, not byte-perfect metering.

---

## Configuration

Pool and memory settings are control-plane config, not hardcoded. Per-tenant overrides can raise or lower limits based on plan/contract.

```yaml
global:
  max_concurrent_fetches: 200
  max_memory_bytes: 1610612736  # 1.5GB

connectors:
  github:
    max_concurrent: 30
    max_memory_bytes: 314572800  # 300MB
    fetch_timeout: 5s
  jira:
    max_concurrent: 10
    max_memory_bytes: 314572800  # 300MB
    fetch_timeout: 8s
  salesforce:
    max_concurrent: 25
    max_memory_bytes: 524288000  # 500MB
    fetch_timeout: 15s
```
