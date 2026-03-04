# Six-Month Execution Plan

## Guiding Principle: Earn Complexity

Every capability starts simple and graduates to its production form only when the simpler version becomes the bottleneck. We ship a working system in Month 1 and harden it into a production system by Month 6.

| What we do | What we defer |
|---|---|
| End-to-end query path from Day 1 | Distributed rate limiting until multi-node is real (M3) |
| Audit logging from Day 1 (structured logs to S3) | Kafka audit pipeline until volume demands it (M4) |
| Access control from Day 1 (config-driven) | Full policy engine (OPA) until policy complexity warrants it (M3) |
| Encryption at rest from M2 (per-tenant keys) | Vault/secrets management until credential count grows (M3) |
| Polling-based config sync (good enough for <50 tenants) | Push-based propagation (OPAL) until revocation latency matters (M5) |

---

## Team

| Role | Count | Ramp |
|---|---|---|
| Eng Manager / Tech Lead | 1 | M1 |
| Backend Engineers | 3 | M1 (2 core + 1 connectors) |
| Infra / Platform Engineer | 1 | M1 half-time, M2 full |
| Security Engineer | 1 | M2 half-time, M3 full |
| QA / Reliability Engineer | 1 | M2 |
| Product Manager | 0.5 | M1 |
| Developer Experience | 0.5 | M3 |

**Peak: ~8 FTE (M3-M6). M1 starts with 4-5.** The EM + 2 backend engineers can deliver M1 unblocked if hiring for other roles slips.

---

## Monthly Roadmap

### M1 — Walking Skeleton

**One tenant. Two connectors. One cross-app query. Demo-able.**

Build the end-to-end path: user submits SQL, system parses it, fans out to GitHub and Jira connectors in parallel, joins results, applies access control (row filtering + column masking), and returns data with freshness and rate-limit metadata.

Rate limiting and caching are in-memory (sufficient for fixed infrastructure). Audit is structured log lines shipped to S3. Observability is four Grafana panels: query latency, connector fetch time, cache hit ratio, rate-limit rejections.

**Exit**: Stakeholder demo of a cross-app join with access control, rate limiting, caching, and basic dashboards.

---

### M2 — Production Foundations

**Real authentication. Real infrastructure. Performance baseline.**

Replace prototype auth with enterprise SSO (OIDC). Stand up infrastructure-as-code (Terraform + Helm) and CI/CD. Introduce per-tenant encryption keys. Build the control plane (tenant registry, connector configs, policies in Postgres). Establish distributed tracing end-to-end. Run first load test at 200 QPS to set a performance baseline.

**Exit**: P95 < 1.8s on single-source queries. Staging environment created from scratch via automation. Traces visible across every hop.

---

### M3 — Policy Engine, Shared State & Async Path

**Scale access control. Handle rate-limit overflow gracefully. Validate connector SDK generality.**

Graduate from config-file policies to an embedded policy engine (OPA). Move rate limiting and caching to Redis (required now that we scale beyond one node). Add the async query path: when a connector's rate budget is exhausted, queue the query and return a job ID for polling. Finalize the error vocabulary so clients get actionable messages. Ship a third connector to prove the SDK works across different auth flows and rate-limit models.

**Exit**: Policy engine denies/masks correctly. Async overflow works end-to-end. Rate limits hold across multiple nodes. Third connector operational.

---

### M4 — Scale & Operational Maturity

**Hit 1,000 QPS. Production-grade deployment. Secure the network.**

Autoscaling, backpressure, and circuit breakers to handle load. Short-lived materialization for large cross-app joins that don't fit in memory. Kafka-based audit pipeline for durable, compliant event delivery. Service mesh (mTLS) for zero-trust networking. Canary deployments with automated rollback. Vault for credential management.

**Exit**: 1k QPS sustained — P50 < 500ms, P95 < 1.5s, error rate < 0.5%. Canary auto-rollback verified. Audit events in S3 within 5 minutes.

---

### M5 — Multi-Tenant Hardening & Cost Controls

**Production multi-tenancy. Alerting. Cost visibility.**

Push-based policy propagation (OPAL) for sub-second access revocation. Automated tenant onboarding (< 5 min to first query) and offboarding with crypto-shredding (disable encryption key = all data unreadable). Alerting suite for latency, errors, rate limits, and infrastructure health. Per-tenant cost dashboards with budget caps. Connector schema versioning. Data residency enforcement.

**Exit**: Policy revocation propagates in < 2 seconds. Tenant lifecycle automated. Alerts fire within 5 minutes of injected failures. Cost tracking accurate per tenant.

---

### M6 — GA Readiness

**Security review. Chaos testing. Runbooks. Sign-off.**

External pen-test and threat model review. Chaos engineering drills: connector failure, Redis partition, Kafka lag, pod OOM, certificate expiry. Disaster recovery validation (multi-AZ failover). Incident runbooks for every known failure mode. SLO dashboard with error budget policy. Single-tenant deployment mode (same codebase, different config). Documentation freeze.

**Exit (GA sign-off)**: Pen-test clean. All chaos drills pass. DR failover < 30 min. 1k QPS with 99.9% availability over 24h. Runbooks reviewed by on-call. Sign-off from EM, Security, PM, QA.

---

## Component Evolution

How each capability matures across the six months:

| Capability | M1-2 (Ship it) | M3-4 (Scale it) | M5-6 (Harden it) |
|---|---|---|---|
| **Authentication** | Token-based (HMAC) → SSO (OIDC) | — | — |
| **Access Control** | Config file + Go engine | Policy engine (OPA) | Push-based revocation (OPAL) |
| **Rate Limiting** | In-memory, single node | Redis-backed, multi-node | Adaptive (reads SaaS API headers) |
| **Caching** | In-memory with freshness hints | Shared cache (Redis) + smart cache reuse | Large-result materialization |
| **Audit** | Structured logs → S3 | — | Kafka pipeline → S3 with compliance retention |
| **Secrets** | Environment variables | Per-tenant encryption keys (KMS) | Vault + automated rotation |
| **Networking** | Plain HTTP | — | Mutual TLS (service mesh) |
| **Deployment** | Docker Compose | Terraform + Helm + CI/CD | Canary deploys, DR validated |
| **Observability** | Metrics + dashboards | Distributed tracing | Alerting + SLO error budgets |
| **Multi-Tenancy** | Single tenant | Namespace isolation | Automated onboarding/offboarding + crypto-shredding |

---

## Risk Register

| Risk | Impact | Mitigation |
|---|---|---|
| **Connector variability** — every SaaS API has unique auth, pagination, rate limits, and schema quirks | High | Capability model in SDK abstracts differences; budget 2 weeks per new connector |
| **Shared API quota exhaustion** — our queries compete with customer's other integrations using the same OAuth token | High | Consume only 80% of stated limit; read real-time remaining budget from response headers; async overflow |
| **Schema drift** — SaaS APIs deprecate fields or change behavior without notice | Medium | Connector health checks detect drift; tenants can pin connector versions; deprecation alerts |
| **Hiring delays** — security or infra engineer unavailable by M2 | Medium | Core team covers basics; contract consultant for pen-test |
| **Large join memory pressure** — cross-app joins with 100K+ rows per side | High | Automatic spill to disk-based engine; query-level row limits; memory monitoring |
| **Compliance scope creep** — new data residency or retention rules mid-project | High | Data residency tags from M5; audit retention configurable; architecture supports region-pinned storage |

---

## Budget

### Infrastructure (AWS)

| Phase | Monthly | Notes |
|---|---|---|
| M1-2 | ~$1,500-2,000 | Dev environment only |
| M3-4 | ~$5,000-8,000 | Add shared cache, staging environment |
| M5-6 | ~$10,000-18,000 | Production cluster, audit pipeline, multi-AZ, encryption at scale |

### Team

| Item | Cost |
|---|---|
| ~8 FTE average over 6 months | ~$720,000 |
| External penetration test (M6) | ~$30,000 |
| **Total 6-month budget** | **~$770,000** |

---

## Milestones at a Glance

| Month | Theme | Key Exit Gate |
|---|---|---|
| **M1** | Walking Skeleton | Demo: cross-app query with access control, rate limiting, caching |
| **M2** | Production Foundations | SSO auth, infra-as-code, CI/CD, P95 < 1.8s, 200 QPS baseline |
| **M3** | Policy & Async | Policy engine, async overflow, shared cache/rate-limit, 3rd connector |
| **M4** | Scale & Operations | 1k QPS, materialization, audit pipeline, mTLS, canary deploys |
| **M5** | Multi-Tenant Hardening | Sub-second revocation, tenant automation, crypto-shredding, cost controls |
| **M6** | GA Readiness | Pen-test, chaos drills, DR, runbooks, 99.9% availability sign-off |

---
---

# Appendix: Detailed Deliverables (Engineering Reference)

> The sections below are the full implementation-level breakdown kept for engineering reference. Merge into the main plan as needed.

---

## M1 — Detailed Deliverables

| # | Item | Owner | Details |
|---|---|---|---|
| 1.1 | Connector SDK v0 | Connector Eng | 4-method interface: `ID()`, `Tables()`, `Schema()`, `Fetch()`. Capability model declares supported tables, pushable predicates, pagination style, auth type. |
| 1.2 | GitHub connector | Connector Eng | OAuth App token. Pull requests table. Pushdown: `repo`, `state`, `created_after`. Pagination via Link headers. |
| 1.3 | Jira connector | Connector Eng | API token (PAT). Issues table. Pushdown: `project`, `status`, `assignee`. JQL-based pagination. |
| 1.4 | SQL parser + planner | Core Eng 1 | vitess-sqlparser; SELECT/WHERE/LIMIT/ORDER BY. Filter classification (partition vs. value vs. identity). QueryPlan struct emitted. |
| 1.5 | Executor with hash join | Core Eng 1 | Concurrent fanout via errgroup. In-process hash join for cross-app queries. Timeout budget (5s default). |
| 1.6 | Entitlement skeleton | Core Eng 2 | YAML policy file. Pre-fetch table-access check. Post-fetch RLS row filter + CLS column mask. JWT claims → Principal → policy evaluation. |
| 1.7 | In-memory rate limiting | Core Eng 2 | `golang.org/x/time/rate` token bucket. Per `(tenant, connector)`. Fixed pod count — divide limit by pod count. Friendly 429 with `Retry-After` and budget fields. |
| 1.8 | In-memory TTL cache | Core Eng 2 | Fetch-level cache keyed on `hash(tenant + connector + table + pushed_predicates)`. `max_staleness` query hint. Response includes `freshness_source`. |
| 1.9 | HTTP gateway | Core Eng 1 | `POST /v1/query`, `GET /healthz`. chi router. JWT middleware (HMAC-SHA256 for now). Structured JSON responses with `trace_id`, `freshness_ms`, `rate_limit_status`. |
| 1.10 | Audit v0 — structured logs | Infra Eng (0.5) | Every query produces a structured log line (zap JSON): `trace_id`, `tenant_id`, `user_id`, `sql`, `tables_accessed`, `rows_returned`, `entitlement_decisions`, `latency_ms`. Logs shipped to S3 via Fluent Bit. |
| 1.11 | Basic observability | Infra Eng (0.5) | Prometheus metrics: `query_duration_seconds`, `connector_fetch_duration_seconds`, `rate_limit_rejections_total`, `cache_hit_ratio`. Grafana dashboard with these 4 panels. |
| 1.12 | Docker Compose dev env | Infra Eng (0.5) | `docker-compose up` runs gateway + Prometheus + Grafana + Jaeger. No external dependencies. |

### What We Deliberately Skip in M1

| Skipped | Why | When It Arrives |
|---|---|---|
| OPA / Rego | YAML policy is sufficient for < 10 policy rules | M3 |
| Redis for rate limiting | In-memory is correct for fixed pod count | M3-4 |
| Redis for caching | In-memory cache is fine for single-pod or low-traffic | M3 |
| Kafka audit pipeline | Structured logs → S3 via Fluent Bit is audit-compliant | M4 |
| Vault / KMS | Env vars + k8s secrets for now | M2-3 |
| mTLS / Istio | Not needed in dev; plain HTTP between co-located services | M4 |
| Async query path | All queries are synchronous | M3 |
| Multi-tenant namespace isolation | Single namespace, single tenant | M4 |
| Terraform | Docker Compose for dev; manual cloud setup if needed | M2 |

### M1 Exit Criteria (Detailed)

- [ ] `POST /v1/query` with `SELECT pr.title, i.summary FROM github_prs pr JOIN jira_issues i ON pr.key = i.key WHERE pr.repo = 'myorg/myrepo' AND i.project = 'PROJ'` returns joined results
- [ ] Same query with a `viewer` role token returns masked columns (CLS) and filtered rows (RLS)
- [ ] Rate limit hit returns 429 with `retry_after_seconds` and `budget_remaining`
- [ ] Cache hit returns results with `freshness_source: "CACHE"` and correct `freshness_ms`
- [ ] Grafana dashboard shows query latency, connector fetch time, cache hit ratio, rate limit rejections
- [ ] Demo to stakeholders with one tenant

---

## M2 — Detailed Deliverables

| # | Item | Owner | Details |
|---|---|---|---|
| 2.1 | OIDC JWT authentication | Core Eng 2 | Replace HMAC JWT with OIDC (Auth0/Okta). JWKS endpoint validation. Token refresh flow for connectors. |
| 2.2 | Predicate pushdown optimization | Core Eng 1 | Planner classifies predicates into 3 categories (partition/value/identity). Broadens time predicates for cache sharing. Subsumption check: skip API call if cached superset exists. |
| 2.3 | Connector auth lifecycle | Connector Eng | OAuth 2.0 refresh token flow for GitHub. Reactive 401 → re-auth. Jira PAT rotation support. Credential storage in k8s Secrets (Vault in M3). |
| 2.4 | Per-tenant KMS (basic) | Security Eng (0.5) | AWS KMS CMK per tenant. Envelope encryption for cached data at rest. Key alias: `alias/queryfed/{tenant_id}/{env}`. Crypto-shred = disable CMK. |
| 2.5 | Terraform v0 | Infra Eng | Modules for: VPC, EKS cluster, RDS Postgres, S3 buckets (audit + cache spill), KMS keys. Environment parity: dev/staging/prod from same modules. |
| 2.6 | Helm chart v0 | Infra Eng | Gateway deployment, HPA stub, ConfigMap for rate-limit + policy configs, ServiceAccount, Ingress. |
| 2.7 | CI/CD pipeline | Infra Eng | GitHub Actions: lint → test → build image → push to ECR → deploy to staging (Helm upgrade). No canary yet (M4). |
| 2.8 | Observability v1 | Infra Eng | OpenTelemetry traces (OTLP → Jaeger). Trace spans: `gateway.handle`, `planner.plan`, `executor.fetch.{connector}`, `executor.join`, `entitlements.check`. Exemplar links from Prometheus metrics to traces. |
| 2.9 | Control plane service | Core Eng 2 | Single Go service + Postgres. Tables: `tenants`, `tenant_connectors`, `rate_limit_configs`, `entitlement_policies`, `schema_catalog`. REST API for CRUD. Data plane polls with 30-60s TTL cache. |
| 2.10 | Load test baseline | QA Eng | k6 script targeting 200 QPS for 60s. Capture P50/P95/P99 latency, error rate, connector call count. Establish baseline for regression tracking. |

### M2 Exit Criteria (Detailed)

- [ ] OIDC tokens from a real IdP authenticate successfully
- [ ] P95 < 1.8s for `SELECT * FROM github_prs WHERE repo = 'X' LIMIT 50` (single-source, predicate pushed)
- [ ] Terraform `plan` + `apply` creates a staging environment from scratch
- [ ] Helm `upgrade` deploys new version with zero manual steps
- [ ] Jaeger shows full trace from gateway → planner → connector → join → response
- [ ] k6 load test at 200 QPS shows < 1% error rate

---

## M3 — Detailed Deliverables

| # | Item | Owner | Details |
|---|---|---|---|
| 3.1 | OPA Go SDK integration | Core Eng 2 + Security Eng | Replace YAML policy engine with embedded OPA (Go SDK, in-process, no network call). Rego policies for RLS/CLS. Policy bundles stored in control plane Postgres, loaded at startup + on poll. |
| 3.2 | Policy DSL | Core Eng 2 | Admin-friendly YAML/JSON that compiles to Rego. Supports: `allow_tables`, `row_filter` (RLS), `column_mask` (CLS), `deny_columns`. Per-connector, per-role. |
| 3.3 | Async query path | Core Eng 1 | When rate limit exhausted and tenant policy allows async: enqueue to SQS (or Redis Stream). Return `{ job_id, status: "queued", poll_url }`. Worker drains queue respecting rate limits. `GET /v1/jobs/{id}` for status + results. |
| 3.4 | Error vocabulary | Core Eng 1 + DX Eng | Finalized codes: `RATE_LIMIT_EXHAUSTED`, `STALE_DATA`, `ENTITLEMENT_DENIED`, `SOURCE_TIMEOUT`, `PARTIAL_RESULTS`, `INVALID_SQL`, `CONNECTOR_ERROR`, `STALENESS_CLAMPED`. Each includes `suggestion` field for client UX. |
| 3.5 | Redis for rate limiting | Infra Eng | Redis Cluster (ElastiCache). Lua scripts for atomic token bucket operations. Fallback to local-state with `limit / pod_count` when Redis unreachable. Reservation-cancel pattern: reserve token before cache check, cancel on cache hit. |
| 3.6 | Redis for caching | Infra Eng | Two-tier: L1 in-memory (per-pod, 100MB cap) → L2 Redis (shared, fetch-level cache). Same cache key scheme. Redis miss → connector fetch → write-back to both tiers. |
| 3.7 | Connector capability model v1 | Connector Eng | Connector declares: supported operations, pushable predicates, rate-limit model (token bucket vs. concurrency semaphore vs. composite), pagination style, auth type. Planner uses capabilities for query planning. |
| 3.8 | Third connector (Salesforce or Slack) | Connector Eng | Validates SDK generality. Different auth flow (OAuth 2.0 with PKCE), different rate-limit model (composite: daily + concurrent). |
| 3.9 | Notification on async completion | Core Eng 1 | Webhook callback or polling. Configurable per tenant. |

### M3 Exit Criteria (Detailed)

- [ ] OPA Rego policy denies a column access and masks a field — verified in integration test
- [ ] Async query completes after rate-limit window: submit → poll → results
- [ ] Error responses include structured `code`, `suggestion`, `retry_after_seconds`, `trace_id`
- [ ] Redis rate limiting correctly prevents burst beyond configured limit across 3 pods
- [ ] Third connector works end-to-end with a different auth and rate-limit model

---

## M4 — Detailed Deliverables

| # | Item | Owner | Details |
|---|---|---|---|
| 4.1 | Autoscaling | Infra Eng | HPA on CPU (70%) + custom metric `in_flight_queries` (>250/pod → scale out). Scale-in: 5min stabilization, max 20% reduction per step. Min 2 / max 20 replicas. |
| 4.2 | Short-lived materialization | Core Eng 1 | When join result set > 10K rows or RSS > 80%: spill to DuckDB temp file (local SSD) or Parquet on S3. Lifecycle: auto-delete after 30 min. Encrypted with tenant DEK. |
| 4.3 | Kafka audit pipeline | Security Eng + Infra Eng | Replace Fluent Bit → S3 with: Gateway → Kafka (async, partitioned by `tenant_id`) → Kafka Connect S3 Sink → S3 WORM Parquet. Separate KMS CMK for audit. 7-year retention. |
| 4.4 | mTLS via Istio | Security Eng + Infra Eng | Service mesh for all inter-service communication. `PeerAuthentication: STRICT`. Application code unchanged (speaks plain HTTP to localhost sidecar). |
| 4.5 | Canary deployments | Infra Eng | Argo Rollouts or Flagger. 5% → 25% → 50% → 100% with automated rollback on P99 latency > 3s or error rate > 2%. |
| 4.6 | Multi-tenant namespace isolation | Infra Eng | K8s namespace per tenant. NetworkPolicies: deny-all default, allow only from gateway. Resource quotas per namespace. |
| 4.7 | Backpressure and overload protection | Core Eng 1 | Global goroutine pool (200/pod). Per-connector caps (GitHub=30, Jira=10). Gateway load shed: >500 in-flight → 503 + `Retry-After`. Circuit breaker per connector: 5 errors in 30s → OPEN → 30s cooldown → HALF → probe → CLOSED. |
| 4.8 | Vault integration | Security Eng | Vault Agent sidecar for connector credentials. Secrets mounted as files. Connector reads from filesystem. Rotation support for OAuth tokens. |
| 4.9 | 1k QPS load test | QA Eng | k6 script: ramp to 1k QPS over 2 min, sustain 60s. Pass criteria: P95 < 1.5s, error rate < 0.5%, no OOM kills. |

### M4 Exit Criteria (Detailed)

- [ ] 1k QPS sustained for 60s: P50 < 500ms, P95 < 1.5s, error rate < 0.5%
- [ ] HPA scales from 2 → N pods under load without manual intervention
- [ ] Materialized join query returns results for 10K+ row joins
- [ ] Kafka audit pipeline delivers events to S3 within 5 minutes
- [ ] Canary deployment auto-rolls back when injecting a bad config
- [ ] `vault kv get` retrieves connector credentials; rotation verified

---

## M5 — Detailed Deliverables

| # | Item | Owner | Details |
|---|---|---|---|
| 5.1 | OPAL for policy propagation | Security Eng | OPA sidecar per pod + OPAL server. Sub-second policy revocation propagation. Replace polling for high-severity changes (entitlement revocation, tenant offboarding). Low-severity changes remain poll-based. |
| 5.2 | Tenant onboarding automation | Core Eng 2 + Infra Eng | API + CLI: create tenant → provision KMS CMK → create k8s namespace → seed rate-limit config → enable connectors. < 5 min from request to first query. |
| 5.3 | Tenant offboarding + crypto-shredding | Security Eng | Disable tenant → cancel all in-flight queries → purge cache → disable KMS CMK → schedule CMK deletion (30-day wait) → delete namespace. All data becomes undecryptable. |
| 5.4 | Alerting suite | QA Eng + Infra Eng | PagerDuty integration. Alerts: P95 > 2s (5 min window), error rate > 2%, rate-limit rejection rate > 30%, connector circuit breaker OPEN, audit pipeline lag > 15 min, pod OOM. |
| 5.5 | Cost guardrails | Core Eng 2 | Per-tenant monthly API call budget. Dashboard showing: API calls by connector, cache hit rate, materialization storage, compute hours. Alert when tenant hits 80% of budget. Hard cap at 100% (configurable: block vs. throttle). |
| 5.6 | Connector schema versioning | Connector Eng | Schema catalog tracks connector versions. Schema migration path: old_version → new_version with field mapping. Tenants can pin connector version. Deprecation notices in query response metadata. |
| 5.7 | Data residency tags | Security Eng | Tenant config specifies allowed regions (e.g., `eu-west-1`). Cache, materialization, and audit data placed in tagged regions. Job scheduler respects residency constraints. |
| 5.8 | Performance profiling | Core Eng 1 | Continuous profiling (Pyroscope or Go pprof). Identify and fix: allocations in hot path, GC pressure from large result sets, connection pool contention. Target: 20% latency improvement from M4 baseline. |
| 5.9 | Admin console v0 | DX Eng | Web UI for: tenant CRUD, connector enable/disable, rate-limit config, policy editor, audit log viewer. Read-only initially; write operations in M6. |

### M5 Exit Criteria (Detailed)

- [ ] OPAL propagates a policy revocation to all pods within 2 seconds
- [ ] Tenant onboarding completes in < 5 minutes via API
- [ ] Crypto-shredding verified: after CMK disable, cached data is unreadable
- [ ] Alerts fire within 5 minutes of injected failures
- [ ] Cost dashboard shows accurate per-tenant API call counts with budget tracking
- [ ] Performance report shows 20% latency improvement over M4

---

## M6 — Detailed Deliverables

| # | Item | Owner | Details |
|---|---|---|---|
| 6.1 | Security review + pen-test | Security Eng | STRIDE threat model. Engage external pen-testers. Fix all critical/high findings. Document accepted risks. |
| 6.2 | Chaos engineering drills | QA Eng | Scenarios: connector timeout (kill sidecar), Redis failure (network partition), Kafka lag (pause consumers), pod OOM (memory bomb), certificate expiry (rotate Istio certs). Each drill has a runbook entry. |
| 6.3 | DR/BCP validation | Infra Eng | Multi-AZ EKS. RDS multi-AZ failover test. S3 cross-region replication for audit. RPO < 1 hour, RTO < 30 minutes. Documented recovery procedure. |
| 6.4 | Runbooks | All | Incident playbooks for: rate-limit flood, connector auth failure, cache stampede, audit pipeline lag, tenant offboarding emergency, KMS key compromise. Each runbook: detection → triage → mitigation → post-mortem template. |
| 6.5 | Onboarding playbook | DX Eng | For new connectors: SDK guide, capability model reference, test harness, example connector. For new tenants: API guide, policy examples, rate-limit tuning guide. |
| 6.6 | GA acceptance tests | QA Eng | Suite covering: functional (all SQL operations, RLS/CLS, joins), performance (1k QPS, P95 < 1.5s), security (entitlement bypass attempts, injection), resilience (circuit breaker, graceful degradation). |
| 6.7 | SLO dashboard + error budget | Infra Eng | 99.9% monthly availability (43 min downtime budget). Error budget policy: < 50% remaining → freeze non-critical deploys; < 25% → all-hands reliability focus. Dashboard: availability, latency SLOs, error budget burn rate. |
| 6.8 | Single-tenant deployment mode | Infra Eng | Same Helm chart with `deployment.mode=single-tenant` values. Dedicated cluster, no namespace sharing, dedicated Redis/Postgres. Terraform module provisions the full stack. |
| 6.9 | Documentation freeze | DX Eng + EM | Architecture doc, API reference, connector SDK guide, operational runbook, security posture doc — all reviewed and finalized. |

### M6 Exit Criteria (GA Sign-Off, Detailed)

- [ ] Pen-test report: zero critical, zero high findings (or documented exceptions with mitigations)
- [ ] All chaos drills pass: system recovers within SLO after each failure injection
- [ ] DR failover test: RTO < 30 min verified
- [ ] 1k QPS sustained: P50 < 500ms, P95 < 1.5s, availability > 99.9% over 24h test
- [ ] Runbooks reviewed by on-call rotation
- [ ] Single-tenant deployment tested end-to-end
- [ ] Sign-off from: EM, Security, PM, QA

---

## Detailed Component Maturity Timeline

| Component | M1 | M2 | M3 | M4 | M5 | M6 |
|---|---|---|---|---|---|---|
| **AuthN** | HMAC JWT | OIDC JWT | — | — | — | — |
| **AuthZ / Entitlements** | YAML + Go engine | — | OPA Go SDK + Rego | — | OPA sidecar + OPAL | — |
| **Rate Limiting** | In-memory token bucket | — | Redis-backed + Lua | — | Adaptive recalibration | — |
| **Caching** | In-memory TTL | Predicate subsumption | L1 in-mem + L2 Redis | Materialization (DuckDB/S3) | — | — |
| **Audit** | Structured logs → S3 (Fluent Bit) | — | — | Kafka → S3 WORM Parquet | — | Verified retention + compliance |
| **Secrets** | Env vars + k8s Secrets | KMS envelope encryption | Vault Agent sidecar | — | Key rotation automated | — |
| **Networking** | Plain HTTP (Docker Compose) | — | — | mTLS (Istio) | — | Pen-tested |
| **Deployment** | Docker Compose | Terraform + Helm + CI/CD | — | Canary (Argo Rollouts) | — | DR validated |
| **Observability** | Prometheus + Grafana (4 panels) | OTel traces (Jaeger) | — | Alerting (PagerDuty) | SLO dashboard + error budget | — |
| **Multi-Tenancy** | Single tenant, single namespace | — | — | K8s namespace isolation | Onboarding/offboarding automation | Single-tenant deploy mode |
| **Connectors** | 2 (GitHub, Jira) | — | 3rd connector | — | Schema versioning | Onboarding playbook |

---

## Detailed Infrastructure Budget (AWS, us-east-1)

| Resource | M1-2 (Dev/Staging) | M3-6 (Staging + Prod) |
|---|---|---|
| EKS cluster | 1 cluster, 3 `m6i.xlarge` nodes | 2 clusters (staging + prod), 5-20 nodes each (autoscaling) |
| RDS Postgres | `db.t3.medium` (single-AZ) | `db.r6g.large` (multi-AZ) |
| ElastiCache Redis | — | `cache.r6g.large` (3-node cluster) |
| S3 | Audit logs only | Audit + materialization + schema artifacts |
| KMS | 1 CMK (dev) | 1 CMK per tenant + 1 for audit |
| Kafka (MSK) | — | 3-broker `kafka.m5.large` |
