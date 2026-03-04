# Universal SQL Gateway

Cross-app federated SQL query layer for enterprise SaaS applications.

## Quickstart

### Prerequisites
- **Docker route**: Docker & Docker Compose only
- **CLI route**: Go 1.24+

---

### Option A: Docker (UI + full observability stack)

```bash
cd deployment/docker
docker-compose up --build
```

| Service | URL |
|---|---|
| Query UI | http://localhost:8080 |
| Jaeger traces | http://localhost:16686 |
| Prometheus metrics | http://localhost:9090 |

Open http://localhost:8080 and click any scenario button (Cross-app JOIN, RLS, CLS, Cache hit, Rate limit burst, etc.) to run a pre-wired demo query.

---

### Option B: CLI (local Go run)

**1. Generate tokens**
```bash
# admin (sees all rows)
JWT_SECRET=dev-secret go run cmd/token-gen/main.go -sub u-1 -tenant t-1 -username alice -email alice@acme.dev -roles admin -expiry 87600h

# developer (RLS: own rows only)
JWT_SECRET=dev-secret go run cmd/token-gen/main.go -sub u-2 -tenant t-1 -username bob -email bob@acme.dev -roles developer -expiry 87600h

# viewer (CLS: email masked)
JWT_SECRET=dev-secret go run cmd/token-gen/main.go -sub u-3 -tenant t-1 -username charlie -email charlie@acme.dev -roles viewer -expiry 87600h
```

**2. Start the gateway**
```bash
JWT_SECRET=dev-secret POLICY_PATH=configs/policy.yaml go run cmd/query-gateway/main.go
```

**3. Run a query**
```bash
curl -s -X POST http://localhost:8080/v1/query \
  -H "Authorization: Bearer <token>" \
  -H "Content-Type: application/json" \
  -d '{
    "sql": "SELECT pr.title, pr.author, i.summary FROM github.pull_requests pr JOIN jira.issues i ON pr.pr_number = i.pr_number WHERE pr.state = '\''open'\''",
    "max_staleness_ms": 30000
  }' | jq .
```

Response includes `rows`, `columns`, `freshness_ms`, `cache_hit`, `rate_limit_status`, and `trace_id`.

---

### Load test (k6)

```bash
k6 run tests/load/query_load_test.js
```

Runs ~500–1k QPS for 60s and reports P50/P95 latency.

## 🔑 Key Trade-offs


### 1. Tenant-Scoped Fetch Cache + Post-Fetch RLS — vs Per-User Cache
Cache is keyed on `(tenant, connector, table, pushed_filters)`, not per-user. The cache intentionally holds data individual users may not see — RLS/CLS filters are applied locally on read, same model as Postgres RLS. **Give**: cache stores rows the requesting user might not be authorized for. **Get**: 1 cache entry serves 10K users → ~70-80% hit rate instead of ~5%, avoids rate-limit exhaustion. See [data-storage-and-cache-strategy.md](docs/data-storage-and-cache-strategy.md).


### 2. In-Process Connectors + Bulkhead Isolation — vs Out-of-Process Microservices
Connectors run in the same Go binary with goroutine pools, memory budgets, circuit breakers, and panic recovery. **Give**: a misbehaving connector can theoretically affect the process (mitigated by bulkheads). **Get**: eliminates ~80ms serialization overhead per join leg (16% of 500ms P50 budget). Every federated engine at this scale (Trino, Presto, DuckDB) does in-process. Out-of-process justified only for untrusted code or different language runtimes. See [data-plane-design.md §2](docs/data-plane-design.md).


### 3. Freshness Floor + Live-Fetch Budget + Graceful Degradation — vs Honoring max_staleness=0
`max_staleness=0` is clamped to a per-connector floor (e.g., 30s). Live fetches are gated by a per-tenant token bucket. When budget is exhausted, stale cache is served with `CACHE_FORCED` transparency — not an error. **Give**: clients cannot guarantee perfectly fresh data. **Get**: one tenant's freshness demand cannot burn rate-limit budget for all tenants sharing the same OAuth token. See [data-storage-and-cache-strategy.md §5](docs/data-storage-and-cache-strategy.md).


### 4. Federated On-The-Fly Join + Size-Triggered S3 Materialization — vs Frequency-Based Materialization
Default is in-memory hash join (build on smaller side). When memory budget is exceeded, DuckDB handles disk-backed execution. Join results are materialized to S3 **only when the result exceeds a size threshold** (~1MB) — there is no frequency counter or hit-rate tracking. **Give**: repeated small-to-medium joins recompute each time (source cache eliminates the costly part — SaaS API calls — so the join itself is <10ms on warm cache). **Get**: no counter-tracking infrastructure, no threshold tuning, no race conditions on threshold crossing — and zero compliance surface for small results. Large results (100K+ rows per side) are the only ones worth persisting, and the size of the result itself is an unambiguous trigger that needs no coordination. See [freshness-and-caching.md §Materialization](docs/data-plane/freshness-and-caching.md).


### 5. OPAL Push for Policy Revocation — vs TTL-Only Propagation
Security-critical changes (entitlement revocation, tenant off-boarding) propagate via OPAL push in ~1-2s. Low-severity changes (new connectors, schema updates) use 30-60s TTL pull. **Give**: OPAL server, sidecar per pod, event bus dependency — real operational complexity. **Get**: revoked user's query window shrinks from 60s to ~1-2s. Most systems accept the TTL window; we chose not to for enterprises with strict revocation SLAs. See [control-plane-design-notes.md §3.2](docs/control-plane-design-notes.md).
