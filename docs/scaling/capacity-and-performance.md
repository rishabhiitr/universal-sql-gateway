# Performance & Autoscaling

> Autoscaling strategy, overload protection, and the load test plan for the data plane.

---

## 1. Autoscaling Strategy

All data plane services are stateless. Scaling is horizontal, automated, and driven by two signals: **CPU utilization** and **in-flight request count**.

### Query Gateway (HPA)

HPA scales on two complementary signals:

1. **CPU utilization** (target ~70%) — catches compute-heavy bursts: large result set marshaling, hash joins on cache hits.
2. **In-flight query count** (custom Prometheus metric) — catches I/O-heavy load. The hot path is network-bound (goroutines blocked on connector responses), so a pod can be at 15% CPU but saturated on memory and connection slots. CPU-based HPA alone would miss this entirely.

Other settings: min 2 replicas (HA across AZs), max 20 (cost guardrail), 5-minute stabilization window on scale-down to prevent flapping.

Cold-start time is ~3s (config pre-warmed from Redis). HPA can add a pod before a 5s query timeout fires.

### Components That Don't Autoscale

| Component | Why | Scaling approach |
|---|---|---|
| **Redis (Source Cache)** | Deployed as a cluster; scales by adding shards if needed. Cache misses degrade gracefully (higher latency, not errors). | Clustered — add shards for capacity |
| **Postgres (Metadata Store)** | Low-QPS config store. Each data plane pod caches config locally with a 30s TTL, so Postgres sees only periodic polls, not query-path traffic. | Read replica if needed, but unlikely given polling model |
| **OPA sidecar** | Runs per-pod with the full policy bundle in-memory (~128 MB). Loopback-only, <1 ms per decision. | Inherits gateway HPA; no independent scaling needed |

---

## 2. Overload Protection

| Trigger | Action | Recovery |
|---|---|---|
| In-flight queries > 500/pod | `503 + Retry-After: 1` — immediate shed, no queueing | Client retries; HPA adds pods |
| Connector: 5 consecutive errors in 30s | Circuit breaker opens → `CONNECTOR_UNAVAILABLE` for 30s | Probe after cooldown; auto-close on success |
| RSS > 80% pod memory | Large joins spill to DuckDB on-disk temp | Automatic; new queries still accepted |
| RSS > 90% pod memory | Reject new queries (`503`) | In-flight queries drain; HPA scales out |

### Per-Query Timeout Budget

```
Total query budget:     5s (configurable per tenant)
 └─ Connector fetch:    4s (leaves 1s for join + filter + marshal)
      └─ Per page:      2s (pagination-heavy fetches get 2s/page)

Context cancellation propagates: killing the query kills all its connector goroutines.
Partial results returned if one source responds and the other times out.
```

---

## 3. Load Test Validation Plan

| Test | Method | Success criteria |
|---|---|---|
| Cache hit rate under real query distribution | k6 script with realistic query mix; measure `cache_hit_ratio` metric | 50–80% hit rate |
| Goroutine pool ceiling | Ramp concurrent queries until RSS hits 80% | Determines `MAX_CONCURRENT_QUERIES` per pod |
| Connector degradation tolerance | Inject 2–4s latency into one connector; measure goroutine pileup and circuit breaker trip time | Timeout budget and backpressure hold without cascade |
| HPA reaction time | Step function from 200 → 1k QPS; measure time-to-stable | Stable within 30s |
