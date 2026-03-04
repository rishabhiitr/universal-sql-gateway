# Universal SQL Query Layer

A federated SQL query engine for enterprise SaaS applications. Query across multiple external systems (GitHub, Jira, Salesforce, etc.) using standard SQL with enterprise-grade security, rate limiting, and compliance.

## 🎯 Project Goal

Build a universal SQL query layer that:
- Allows SQL queries across 1000s of SaaS app types
- Enforces row/column-level security (RLS/CLS)
- Handles rate limits and freshness controls
- Scales to 100s of customers and millions of users
- Supports multi-tenant and single-tenant deployments

## 📋 Deliverables Checklist

- [ ] Design Document (`docs/DESIGN.md`)
- [ ] Six-Month Execution Plan (`docs/EXECUTION_PLAN.md`)
- [ ] Working Prototype (GitHub ↔ Jira scenario)
- [ ] Docker Setup (`deployment/docker/docker-compose.yml`)
- [ ] Load Tests (`tests/load/k6-script.js`)
- [ ] Observability Screenshots (`observability/screenshots/`)
- [ ] README with Quickstart (this file)

## 🏗️ Architecture Overview

```
┌─────────────────────────────────────────────────────────┐
│                     Control Plane                        │
│  (Tenant Registry, Schema Catalog, Policy Store)        │
└─────────────────────────────────────────────────────────┘
                            │
                            ▼
┌─────────────────────────────────────────────────────────┐
│                      Data Plane                          │
│                                                          │
│  ┌──────────────┐  ┌──────────────┐  ┌──────────────┐  │
│  │Query Gateway │→ │Query Planner │→ │  Connectors  │  │
│  │(Auth/AuthZ)  │  │(Pushdown)    │  │(GitHub/Jira) │  │
│  └──────────────┘  └──────────────┘  └──────────────┘  │
│         │                 │                  │          │
│         ▼                 ▼                  ▼          │
│  ┌──────────────┐  ┌──────────────┐  ┌──────────────┐  │
│  │Entitlements  │  │Rate Limiter  │  │Cache/Fresh   │  │
│  │(RLS/CLS)     │  │(Token Bucket)│  │(TTL Control) │  │
│  └──────────────┘  └──────────────┘  └──────────────┘  │
└─────────────────────────────────────────────────────────┘
```

See `docs/DESIGN.md` for detailed architecture.

## 🚀 Quickstart

### Prerequisites
- Go 1.21+
- Docker & Docker Compose (optional)

### Run Locally

```bash
# Install dependencies
go mod download

# Run the query gateway
go run cmd/query-gateway/main.go

# Open the interactive query UI
# http://localhost:8080/query-ui

# In another terminal, test a query
curl -X POST http://localhost:8080/v1/query \
  -H "Authorization: Bearer user-token-123" \
  -H "Content-Type: application/json" \
  -d '{
    "sql": "SELECT * FROM github.pull_requests WHERE state = '\''open'\'' LIMIT 10",
    "max_staleness_ms": 60000
  }'
```

### Run with Docker

```bash
# Build and start all services
cd deployment/docker
docker-compose up --build

# Open interactive UI in your browser
# http://localhost:8080/query-ui
```

## 📊 Example Queries

```sql
-- Single source query
SELECT id, title, state, created_at
FROM github.pull_requests
WHERE repo = 'my-org/my-repo' AND state = 'open'
LIMIT 20

-- Cross-app join (GitHub ↔ Jira)
SELECT
  gh.pr_number,
  gh.title as pr_title,
  jira.issue_key,
  jira.status as jira_status
FROM github.pull_requests gh
JOIN jira.issues jira ON gh.jira_issue_id = jira.issue_key
WHERE gh.repo = 'my-org/my-repo'
  AND gh.created_at > '2024-01-01'
```

## 🧪 Testing

```bash
# Run unit tests
go test ./...

# Run with coverage
go test -cover ./...

# Run load tests (requires k6)
cd tests/load
k6 run k6-script.js
```

## 🔑 Key Trade-offs

### 1. Tenant-Scoped Fetch Cache + Post-Fetch RLS — vs Per-User Cache
Cache is keyed on `(tenant, connector, table, pushed_filters)`, not per-user. The cache intentionally holds data individual users may not see — RLS/CLS filters are applied locally on read, same model as Postgres RLS. **Give**: cache stores rows the requesting user might not be authorized for. **Get**: 1 cache entry serves 10K users → ~70-80% hit rate instead of ~5%, avoids rate-limit exhaustion. See [data-storage-and-cache-strategy.md](docs/data-storage-and-cache-strategy.md).

### 2. In-Process Connectors + Bulkhead Isolation — vs Out-of-Process Microservices
Connectors run in the same Go binary with goroutine pools, memory budgets, circuit breakers, and panic recovery. **Give**: a misbehaving connector can theoretically affect the process (mitigated by bulkheads). **Get**: eliminates ~80ms serialization overhead per join leg (16% of 500ms P50 budget). Every federated engine at this scale (Trino, Presto, DuckDB) does in-process. Out-of-process justified only for untrusted code or different language runtimes. See [data-plane-design.md §2](docs/data-plane-design.md).

### 3. Freshness Floor + Live-Fetch Budget + Graceful Degradation — vs Honoring max_staleness=0
`max_staleness=0` is clamped to a per-connector floor (e.g., 30s). Live fetches are gated by a per-tenant token bucket. When budget is exhausted, stale cache is served with `CACHE_FORCED` transparency — not an error. **Give**: clients cannot guarantee perfectly fresh data. **Get**: one tenant's freshness demand cannot burn rate-limit budget for all tenants sharing the same OAuth token. See [data-storage-and-cache-strategy.md §5](docs/data-storage-and-cache-strategy.md).

### 4. Federated On-The-Fly Join + DuckDB Spill — vs Pre-Materialization
Default is in-memory hash join (build on smaller side). When memory budget is exceeded, DuckDB handles disk-backed execution. No join results are persisted. **Give**: repeated expensive joins recompute each time (source cache eliminates the costly part — SaaS API calls — so the join itself is <10ms on warm cache). **Get**: zero storage cost, no compliance surface from storing cross-source joined data, no materialized-view invalidation problem. See [DESIGN.md §6](docs/DESIGN.md).

### 5. OPAL Push for Policy Revocation — vs TTL-Only Propagation
Security-critical changes (entitlement revocation, tenant off-boarding) propagate via OPAL push in ~1-2s. Low-severity changes (new connectors, schema updates) use 30-60s TTL pull. **Give**: OPAL server, sidecar per pod, event bus dependency — real operational complexity. **Get**: revoked user's query window shrinks from 60s to ~1-2s. Most systems accept the TTL window; we chose not to for enterprises with strict revocation SLAs. See [control-plane-design-notes.md §3.2](docs/control-plane-design-notes.md).

---

<details>
<summary><strong>Other Notable Design Decisions</strong> (component-level, not system-shaping)</summary>

- **Connector-declared rate-limit model**: SaaS APIs enforce limits differently (budget-based, concurrency-slot, composite). Each connector declares its model; the framework instantiates the correct limiter. Not a tradeoff — just correct modeling.
- **Redis for external API limits (L3/L4)**: External SaaS budgets must be globally accurate across pods. Internal fairness (L1/L2) also uses Redis but can tolerate longer sync intervals.
- **Two-tier TTL (soft + hard) with ETag revalidation**: Soft TTL serves stale + background revalidation (zero added latency). Hard TTL forces synchronous re-fetch. ETag 304s are near-free against rate-limit budget.
- **Reservation pattern for rate-limit tokens**: Token is reserved pre-cache-check, cancelled on cache hit, committed on miss. Cache hits don't burn external API budget.
- **Envelope encryption with per-tenant KMS keys**: Reduces KMS calls to one per data key (not per row). Per-tenant CMK enables crypto-shredding on off-boarding.
- **Vault Agent sidecar for credentials, KMS SDK embedded**: Different failure-domain requirements. Vault crash doesn't take down query serving; KMS on hot path can't tolerate sidecar latency.

</details>

## 📁 Project Structure

```
.
├── cmd/
│   └── query-gateway/          # Main entry point
├── internal/
│   ├── gateway/                # HTTP API handlers
│   ├── planner/                # SQL parser & query planner
│   ├── connectors/             # Connector implementations
│   │   ├── github/
│   │   └── jira/
│   ├── entitlements/           # RLS/CLS policy engine
│   ├── ratelimit/              # Token bucket rate limiter
│   ├── cache/                  # Freshness control & caching
│   └── models/                 # Domain models
├── pkg/
│   ├── errors/                 # Error vocabulary
│   └── middleware/             # HTTP middleware
├── docs/
│   ├── DESIGN.md               # Architecture design doc
│   ├── EXECUTION_PLAN.md       # 6-month roadmap
│   └── diagrams/               # Architecture diagrams
├── tests/
│   └── load/                   # Load testing scripts
├── deployment/
│   ├── docker/                 # Docker setup
│   └── k8s/                    # Kubernetes manifests (future)
└── observability/
    ├── prometheus.yml          # Metrics config
    └── screenshots/            # Trace screenshots
```

## 🎯 Prototype Scope

For the take-home assignment, we implement:

✅ **Implemented**:
- Query Gateway with auth
- SQL parser (basic SELECT/WHERE/LIMIT)
- 2 connectors (GitHub + Jira, mocked for simplicity)
- RLS enforcement (user role → filtered rows)
- Rate limiting (token bucket)
- Freshness control (cache with TTL)
- Observability (Prometheus metrics + trace)

⚪ **Future Work** (documented but not implemented):
- Complex JOINs with predicate pushdown
- Column-level security (CLS)
- Async query execution for long-running queries
- Real OAuth flows (using mocked tokens for now)
- Multi-region deployment
- Materialized views for complex aggregations

## 📚 Documentation

- [Design Document](docs/DESIGN.md) - Architecture, components, security model
- [Execution Plan](docs/EXECUTION_PLAN.md) - 6-month roadmap with milestones
- [API Reference](docs/API.md) - HTTP API endpoints and schemas
- [Connector Development Guide](docs/CONNECTORS.md) - How to add new connectors

## 🤝 Contributing

This is a take-home assignment project. For questions, contact:
- `souvik-sen@ema.co`
- `careers@ema.co`

## 📄 License

MIT License (for take-home assignment purposes)
