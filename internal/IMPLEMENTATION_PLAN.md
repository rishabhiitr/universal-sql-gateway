# Plan: Universal SQL Query Layer - Go Prototype

## Context
Build a working Go prototype for an Eng Lead take-home: a universal SQL query layer querying across SaaS apps. Scenario: **GitHub PRs + Jira Issues** cross-app join. The repo is a pure skeleton (directory structure + README, zero Go files). Everything is built from scratch.

## Scenario & Demo Query
```sql
SELECT gh.title, gh.state, j.issue_key, j.status, j.assignee
FROM github.pull_requests gh
JOIN jira.issues j ON gh.jira_issue_id = j.issue_key
WHERE gh.state = 'open'
LIMIT 10
```

## Progress Tracker

Last updated: 2026-02-28

### Layer Status
- [x] Layer 0: Foundation
- [x] Layer 1: Rate Limiting + Cache
- [x] Layer 2: Entitlements
- [x] Layer 3: Connector Interface + Implementations
- [x] Layer 4: SQL Planner + Executor
- [x] Layer 5: HTTP Layer
- [x] Layer 6: Entry Point
- [x] Layer 7: Ops & Tests

### Milestones Logged
- **2026-02-28 - Layer 0 complete**
  - Added `pkg/errors/errors.go` with typed `QueryError`, standardized error codes, `RetryAfter`, `Source`, and `HTTPStatus()` mapping.
  - Added `internal/models/models.go` with shared domain models for request/response, planning, joins, filters, and source metadata.
- **2026-02-28 - Layer 1 complete**
  - Added `internal/ratelimit/limiter.go` with lazy-initialized per `(tenant, connector)` token buckets and retry-after calculation.
  - Added `internal/cache/cache.go` with in-memory TTL storage, per-query staleness checks in `Get`, and background eviction with clean shutdown.
- **2026-02-28 - Layer 2 complete**
  - Added `configs/policy.yaml` baseline policy for role-based table access, row filters, and column masking.
  - Added `internal/entitlements/policy.go` YAML policy loader.
  - Added `internal/entitlements/engine.go` with `CheckTableAccess`, `ApplyRLS`, and `ApplyCLS` (RLS before CLS).
- **2026-02-28 - Layer 3 complete**
  - Added `internal/connectors/connector.go` with small `Connector` interface and connector registry.
  - Added `internal/connectors/github/connector.go` with realistic seeded PR data, simulated latency, and filter pushdown.
  - Added `internal/connectors/jira/connector.go` with realistic seeded issue data, simulated latency, and filter pushdown.
- **2026-02-28 - Layer 4 complete**
  - Added `internal/planner/parser.go` using `vitess-sqlparser` to parse SELECT queries, JOINs, filters, ORDER BY, and LIMIT into `QueryPlan`.
  - Implemented filter classification (`classifyFilters`) to split connector pushdown filters vs post-fetch filters.
  - Added `internal/planner/executor.go` with concurrent connector fanout (`errgroup`), hash join, cache/rate-limit orchestration, and RLS/CLS application.
  - Added `internal/planner/parser_test.go` covering single-source parsing, JOIN parsing, ORDER BY handling, and invalid SQL rejection.
- **2026-02-28 - Layer 5 complete**
  - Added `pkg/middleware/auth.go` with JWT (HMAC-SHA256) validation and typed context principal propagation.
  - Added `pkg/middleware/observability.go` with Prometheus request/latency/rate-limit/cache metrics and HTTP middleware.
  - Added `internal/gateway/handler.go` with `POST /v1/query`, `GET /healthz`, centralized `writeError()`, and `trace_id` response wiring.
- **2026-02-28 - Layer 6 complete**
  - Added `cmd/query-gateway/main.go` with explicit dependency injection, middleware + routes, and graceful shutdown handling.
  - Added `cmd/token-gen/main.go` to generate test JWTs for role-based testing.
- **2026-02-28 - Layer 7 complete**
  - Added `deployment/docker/Dockerfile` (multi-stage Go build -> distroless runtime, `CGO_ENABLED=0`).
  - Added `deployment/docker/docker-compose.yml` for query-gateway + Jaeger + Prometheus.
  - Added `observability/prometheus.yml` scrape config for gateway metrics.
  - Added `tests/load/k6-script.js` for mixed-query load (single source + JOIN + cached).
  - Added `internal/gateway/handler_test.go` integration tests (success, entitlement denied, rate-limit, cache hit, invalid SQL, missing auth).

### Validation Snapshot (2026-02-28)
- `go test ./...` passes.
- `go build -buildvcs=false ./...` passes (`-buildvcs=false` used because this workspace is not a git repo).

## Build Order (dependency layers)

### Layer 0: Foundation (no internal deps)
1. **`pkg/errors/errors.go`** - Error vocabulary: `RATE_LIMIT_EXHAUSTED`, `STALE_DATA`, `ENTITLEMENT_DENIED`, `SOURCE_TIMEOUT`, `INVALID_QUERY`. Typed `QueryError` struct with `RetryAfter`, `Source`, `HTTPStatus()` method.
2. **`internal/models/models.go`** - All domain types: `Principal`, `QueryRequest`, `QueryResponse`, `QueryPlan`, `SourceQuery`, `JoinSpec`, `FilterExpr`, `Column`, `Row`, `RateLimitStatus`, `SourceMeta`.

### Layer 1: Rate Limiting + Cache
3. **`internal/ratelimit/limiter.go`** - Per (tenant, connector) token bucket using `golang.org/x/time/rate`. Lazy init with `sync.RWMutex` double-check pattern. Returns `QueryError` with `RetryAfter` from `rate.Reservation.Delay()`.
4. **`internal/cache/cache.go`** - In-memory TTL cache. Key = hash(connector+table+filters+tenantID). `Get(key, maxStaleness)` - staleness passed per-query, not stored in cache. Background eviction goroutine with `stop` channel for clean shutdown.

### Layer 2: Entitlements
5. **`configs/policy.yaml`** - Policy config: allowed_roles per table, row_filters (developers see own PRs only), column masking (email redacted for non-admins).
6. **`internal/entitlements/policy.go`** - YAML loader for policy config.
7. **`internal/entitlements/engine.go`** - `CheckTableAccess()`, `ApplyRLS()` (filter rows), `ApplyCLS()` (mask columns). RLS runs first, then CLS.

### Layer 3: Connector Interface + Implementations
8. **`internal/connectors/connector.go`** - `Connector` interface: `ID()`, `Schema(table)`, `Fetch(ctx, principal, sourceQuery)`, `Tables()`. Small interface, Go idiomatic.
9. **`internal/connectors/github/connector.go`** - Mock GitHub connector. Pre-generates ~200 realistic fake PRs. `JiraIssueID` field = `"PROJ-100"` to `"PROJ-199"` (overlaps with Jira for JOIN demo). Simulates configurable latency. Applies pushdown filters in-memory.
10. **`internal/connectors/jira/connector.go`** - Mock Jira connector. Same pattern. `IssueKey` = `"PROJ-100"` to `"PROJ-199"`.

### Layer 4: SQL Planner + Executor
11. **`internal/planner/parser.go`** - Wraps `blastrain/vitess-sqlparser`. `ParseSQL(sql) -> QueryPlan`. Key logic: `classifyFilters()` splits pushdown-eligible (single-table) from post-fetch (cross-table/JOIN) filters. Validates SELECT-only. **Production note**: Would migrate to an ANTLR4-based grammar (as used by Trino/Presto) for full SQL dialect control, custom AST visitors, and extensibility to DDL/DML if needed.
12. **`internal/planner/executor.go`** - The core engine. `Execute(ctx, plan, req) -> QueryResponse`.
    - `fetchConcurrent()`: goroutine per connector via `errgroup`, structured cancellation
    - `hashJoin()`: build hash table from smaller side, probe with larger. O(n+m)
    - Orchestration: check entitlements -> check rate limit -> check cache -> fetch -> join -> RLS -> CLS -> cache set -> order/limit -> response

### Layer 5: HTTP Layer
13. **`pkg/middleware/auth.go`** - JWT parsing middleware. Unexported `contextKey` type (prevents collisions). Stores `*Principal` in context. HMAC-SHA256 validation.
14. **`pkg/middleware/observability.go`** - Prometheus `Metrics` struct (request count, latency histograms by method/path, connector latency, rate limit counter, cache hits). OTel span injection. Structured request logging with zap.
15. **`internal/gateway/handler.go`** - `POST /v1/query` handler + `GET /healthz`. Single `writeError()` function maps `QueryError` -> HTTP status + JSON + `Retry-After` header. `trace_id` in every response.

### Layer 6: Entry Point
16. **`cmd/query-gateway/main.go`** - Explicit dependency injection, zero globals. Init sequence: logger -> tracer -> policy -> metrics -> cache -> rate limiter -> entitlements -> connectors -> executor -> handler -> routes -> HTTP server. Graceful shutdown via `signal.NotifyContext` + `server.Shutdown()`.
17. **`cmd/token-gen/main.go`** - Small CLI to generate test JWT tokens for different roles. Used in README quickstart.

### Layer 7: Ops & Tests
18. **`deployment/docker/Dockerfile`** - Multi-stage: `golang:1.24-alpine` builder -> `distroless/static` runtime. `CGO_ENABLED=0`.
19. **`deployment/docker/docker-compose.yml`** - Services: query-gateway, jaeger (traces UI:16686, OTLP:4318), prometheus (scrape gateway:9090).
20. **`observability/prometheus.yml`** - Scrape config for query-gateway.
21. **`tests/load/k6-script.js`** - Ramp to ~500 VUs over 60s. Mix of single-table, JOIN, and cached queries. Three test tokens (admin, developer, viewer). Custom k6 metrics for rate_limit_hits, cache_hit_rate.
22. **`internal/gateway/handler_test.go`** - Integration tests via `httptest`: success, entitlement denied, rate limit exhausted, cache hit, invalid SQL, missing auth.
23. **`internal/planner/parser_test.go`** - Unit tests for SQL parsing: single table, JOIN, ORDER BY, invalid SQL.

## Key Dependencies (go.mod)
- `github.com/go-chi/chi/v5` - routing (net/http compatible, no magic)
- `github.com/blastrain/vitess-sqlparser` - SQL parsing
- `github.com/golang-jwt/jwt/v5` - JWT
- `golang.org/x/time` - token bucket rate limiter
- `golang.org/x/sync` - errgroup for goroutine fanout
- `gopkg.in/yaml.v3` - policy config
- `github.com/google/uuid` - trace IDs
- `github.com/prometheus/client_golang` - metrics
- `go.opentelemetry.io/otel` + SDK + OTLP exporter - tracing
- `go.uber.org/zap` - structured logging
- `github.com/stretchr/testify` - test assertions

## Go Patterns Showcased
- **Interfaces**: small `Connector` interface (4 methods), capability extension via interface composition
- **Goroutines + errgroup**: concurrent connector fanout with structured cancellation
- **Context propagation**: timeouts, cancellation, principal passing
- **sync.RWMutex**: double-check locking in rate limiter registry
- **Unexported context keys**: `type contextKey int` prevents collisions
- **Typed errors with Is()**: `QueryError` checked by code, not message
- **Graceful shutdown**: `signal.NotifyContext` + server drain + goroutine cleanup

## Verification
1. `go build ./...` - compiles
2. `go test ./...` - unit + integration tests pass
3. `docker-compose up` - starts gateway + jaeger + prometheus
4. `curl` with admin token -> success with all columns visible
5. `curl` with developer token -> masked email columns, filtered rows
6. `curl` with no token -> 401
7. Rapid-fire requests -> 429 with Retry-After header
8. Same query twice -> second response has `cache_hit: true`, `freshness_ms > 0`
9. `k6 run tests/load/k6-script.js` -> ~500 QPS sustained, <1% error rate
10. Jaeger UI at :16686 -> trace showing connector fanout spans
11. Prometheus at :9090 -> `query_duration_seconds` histogram populated
