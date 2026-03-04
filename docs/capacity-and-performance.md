# Capacity & Performance Design

> Part of the Universal SQL Query Layer design. Cross-references: `docs/data-plane-design.md` (goroutine pool internals), `docs/rate-limit-design-notes.md` (token bucket sizing).

---

## 1. Sizing Math at 1k QPS

### CPU

```
CPU work per query (hot path, cache hit):
  SQL parsing:          ~100 μs
  Filter classification: ~10 μs
  Rate-limit check:      ~1 μs
  Cache lookup:          ~5 μs
  JSON unmarshal:       ~1–5 ms  (per 1,000 rows from SaaS API)
  Hash join:            ~0.5 ms  (per 1,000 rows)
  RLS/CLS filter:       ~0.1 ms  (per 1,000 rows)
  JSON marshal:         ~1–5 ms  (for HTTP response)
  ─────────────────────────────
  Total CPU/query:      ~5–15 ms (mid-point: 10 ms)

At 1,000 QPS:
  1,000 × 10 ms = 10,000 ms = 10 CPU-cores/s
  +20% headroom (GC, scheduling) → 12 cores needed
```

Note: connector calls are I/O wait — they consume goroutines, not CPU. CPU is the bottleneck only on cache misses with large result sets.

### Memory

```
Per concurrent query:
  ~1,000 rows × ~500 bytes/row × 2 sources = ~1 MB

100 concurrent queries in-flight:        ~100 MB
Redis cache (L2, in-process mirror):     ~500 MB
Go runtime + code + misc overhead:       ~300 MB
────────────────────────────────────────────────
Total per pod:                           ~1.5–2 GB
```

### Replica Count (Baseline)

| Resource | Calculation | Per-pod target | Baseline pods |
|---|---|---|---|
| **CPU** | 10 ms/query × 1k QPS = 10 cores | 4 cores/pod | 3 pods |
| **Memory** | ~2 GB/pod | 4 GB pod | 3 pods |
| **HA minimum** | 2 across AZs | — | min=2 |
| **Headroom** | 20% above baseline | — | 4–5 pods |

Cache hit ratio assumption: **40% Redis hit rate** halves live connector calls and roughly halves CPU+bandwidth. At 0% cache hits (cold start / bypass), double the pod count.

### Network

```
1,000 QPS × ~100 KB avg response = ~100 MB/s egress
Standard 1 Gbps pod NIC is sufficient.
Connector ingress (SaaS API responses): similar order — dominated by pagination page sizes.
```

---

## 2. Autoscaling

### Query Gateway & Query Executor (HPA)

These are the hot-path stateless services. They scale together (or can be split if gateway becomes a pure L7 proxy).

```yaml
# Example HPA config
metrics:
  - type: Resource
    resource:
      name: cpu
      target:
        type: Utilization
        averageUtilization: 70          # scale out above 70%
  - type: Pods
    pods:
      metric:
        name: in_flight_rps             # custom metric from Prometheus adapter
      target:
        type: AverageValue
        averageValue: "250"             # >250 RPS/pod → add a pod

scaleDown:
  stabilizationWindowSeconds: 300       # wait 5 min before scaling in (avoid flapping)
  policies:
    - type: Percent
      value: 20                         # remove max 20% of pods per step

minReplicas: 2    # always ≥2 for HA across AZs
maxReplicas: 20
```

Pod cold-start time is ~3 s (config cache pre-warmed from Redis on init). This is fast enough for traffic bursts — HPA can add a pod before a 5 s query timeout fires.

### Connector Worker Pods (Out-of-Process Topology)

Applicable only if the connector pool is deployed as separate sidecar or worker pods (see `docs/data-plane-design.md §2`). In the default in-process topology, connector workers scale with the executor pod.

```
Scale metric: Kafka consumer lag per connector topic
Scale out:    lag > 100 messages for 30 s
Scale to zero: lag = 0 for 10 min (off-hours cost saving)
```

### Redis Cache (ElastiCache)

No autoscaling. Redis is vertically scaled and sized for the working set (≤1 GB initially). Cache misses degrade gracefully to live fetch — they don't cause errors, just higher latency and load.

### Control Plane (Tenant Registry, Policy Store, OPA)

Stateless API pods behind an ALB. HPA on CPU (target 60%). Postgres read replicas absorb config read traffic. The control plane does **not** scale with query QPS — data plane pods cache config locally with a short TTL.

---

## 3. Backpressure

The system enforces two tiers of concurrency limits to prevent connector overload and head-of-line blocking.

### Tier 1 — Pod Global Concurrency Pool

```
Max 200 concurrent connector fetches across all connectors on one pod.

When pool is full:
  New fetch request → blocks (waits) until a slot frees or query timeout fires.
  Timeout fires first → partial result or SOURCE_TIMEOUT error returned to caller.
  No unbounded queue — the query context deadline is the queue bound.
```

### Tier 2 — Per-Connector Caps (subset of global pool)

```
GitHub:      max 30 concurrent fetches
Jira:        max 10 concurrent fetches
Salesforce:  max 25 concurrent fetches
(Others):    configurable 5–50 range in connector YAML capability declaration

Sum of per-connector caps intentionally exceeds global cap (30+10+25+... > 200).
The global cap is the hard ceiling; per-connector caps prevent one connector
monopolising the global pool.
```

**Why block instead of drop?** Dropping at the goroutine pool level would return an error to the caller even though the pod has spare capacity to serve the request shortly. Blocking respects the caller's timeout budget and maximises throughput under load. The gateway's in-flight query cap (§4) is the drop-or-reject boundary.

### Head-of-Line Blocking Avoidance

Each connector fetch runs in its own goroutine. A slow Jira call (e.g., 4 s for a large page) does not block a GitHub call in the same query — both run concurrently via `errgroup`. The per-connector cap only limits *new* concurrent fetches, not in-flight ones.

---

## 4. Overload Protection

### Gateway Load Shed

```
In-flight query count > MAX_CONCURRENT_QUERIES (default: 500)
→ Immediately return: 503 Service Unavailable
              headers: Retry-After: 1
              body:    {"error": "SOURCE_TIMEOUT", "message": "Gateway at capacity, retry in 1s"}

No queueing. No head-of-line blocking at the gateway.
```

Rationale: A 500-query queue with 1 s average latency already has 500 queries × 1 s = 500 s of work buffered. Adding more just increases tail latency. Better to reject fast and let the caller retry or route to a different pod.

### Circuit Breaker (Per Connector)

```
State machine per (pod, connector):

CLOSED → OPEN:  5 consecutive errors within 30 s window
OPEN   → HALF:  after 30 s cooldown, allow 1 probe request
HALF   → CLOSED: probe succeeds
HALF   → OPEN:   probe fails

While OPEN:
  All requests to that connector immediately return CONNECTOR_UNAVAILABLE.
  Other connectors in the same query are unaffected.
```

Circuit breaker protects the goroutine pool from accumulating blocked goroutines on a dead upstream. It's distinct from the timeout budget — a connector can be within timeout but still trip the circuit breaker if it consistently returns 5xx errors.

### Per-Query Timeout Budget

```
Default total query budget: 5 s (configurable per tenant via policy)
  └─ Connector fetch budget: 4 s (leaves 1 s for join + filter + marshal)
       └─ Per-page fetch:    2 s (if pagination is needed, each page gets 2 s)

Context cancellation is propagated: cancelling the query context immediately
stops all in-flight connector goroutines for that query.
```

Partial results: if one connector responds within budget and the other times out, the gateway returns the available rows with `partial: true` in the response metadata and `rate_limit_status: SOURCE_TIMEOUT` for the timed-out source.

### Memory Pressure

```
RSS > 80% of pod memory limit
→ In-progress joins that exceed row threshold spill to DuckDB on-disk temp file.
  New queries continue to be accepted.

RSS > 90% of pod memory limit
→ New query requests rejected: 503 + Retry-After: 2
  In-flight queries continue to completion (already past the point of no return).
```

Memory pressure triggers are checked every 500 ms via a background goroutine reading `/proc/self/status`. Spill-to-disk is rare in practice — at 100 concurrent queries × 1 MB = 100 MB working set, a 2 GB pod has 10× headroom.

---

## 5. Summary

| Concern | Mechanism | Threshold |
|---|---|---|
| CPU overload | HPA scale-out | >70% CPU or >250 RPS/pod |
| Memory overload | Spill to DuckDB / reject new queries | >80% / >90% RSS |
| Connector overload | Per-connector goroutine cap | GitHub:30, Jira:10, … |
| Pod overload | Gateway in-flight cap + 503 shed | >500 concurrent queries |
| Connector failure | Circuit breaker | 5 consecutive errors → open 30 s |
| Slow connector | Per-query timeout budget | 4 s fetch budget; 5 s total |
| Scale-in flapping | HPA stabilisation window | 5 min cooldown before scale-in |
| Off-hours cost | Scale-to-zero (async workers) | 10 min of zero lag |
