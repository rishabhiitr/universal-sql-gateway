# Six-Month Execution Plan

## Guiding Principles

| Principle | What it means |
|---|---|
| **Security is not deferred** | KMS and audit pipeline are M2 — they are table stakes for any enterprise pilot, not scale features |
| **One engineer, one domain** | Each engineer owns a domain end-to-end for the full six months. No context switching, no handoffs |
| **Managed over self-hosted** | Use managed services (EKS managed control plane + node groups, managed Kafka — MSK or Confluent Cloud, CodePipeline) instead of self-hosting cluster infrastructure, Kafka, or CD control planes. Core differentiation is the query layer and connector SDK, not infrastructure operations |

---

## Team

| Role | Count | Domain Ownership | Ramps |
|---|---|---|---|
| Tech Lead — Data Plane | 1 | Connectors, executor, rate limiting, caching, materialization | M1 |
| Tech Lead — Control Plane | 1 | Tenant registry, connector configs, OPA/OPAL, policy engine | M1 |
| Staff Engineer | 1 | Seeds hardest design problems: query planner interfaces (M1) + OPA SDK integration (M1) → hands off to L4s once contracts are established | M1 |
| Backend Engineer — L4 | 1 | Connector SDK + all connectors (takes over from TL Data Plane once SDK pattern is set) | M1 |
| Backend Engineer — L4 | 1 | Query planner + parser (takes over from Staff once interfaces are defined) | M1 |
| Security Engineer | 1 | KMS, audit pipeline, secrets, compliance | M2 |
| Infra Engineer | 1 | EKS, node pools, Terraform, managed pipeline | M1 half-time, M2 full |
| QA Engineer | 1 | Load tests, failure drills, runbooks | M3 |
| Product Manager | 1 | Customer interviews, design partner feedback, market research | M1 |

**M1 starts with 5–6 people (2 TL + Staff + 2 L4 + Infra half-time). Security and QA ramp when their domain becomes active.**

**L4 growth path**: L4s who demonstrate strong ownership can take on larger components — rate limit service, Redis caching, or async query path — as those domains mature in M4.

---

## Monthly Roadmap

### M1 — Walking Skeleton

**One tenant. Two connectors. One cross-app query. End-to-end path works.**

Single microservice. Everything configured in YAML — tenants, policies, connector configs. Not production-ready; not for sale. But demo-ready — the end-to-end path works well enough to show a design partner or internal stakeholder what the product does.

- **Query path**: SQL parse → fan-out to GitHub + Jira connectors in parallel → join results → apply access control → return with metadata
- **OPA Go SDK embedded from day one**: policies are `.rego` files loaded from disk at container startup. Simple rules: row filters, column masks, role checks. No bespoke Go authorization logic — OPA from the start so M5 is a config change, not a rewrite
- **Circuit breakers per connector from day one**: not a scale concern, a correctness concern. One flaky API must not stall all queries
- **Rate limiting + upstream throttling**: in-memory token bucket, sufficient for the initial single-cluster deployment. On upstream `429`s or transient throttling, use capped exponential backoff; if budget is still exhausted after retries, fail fast with a clear actionable error. No shared cache yet
- **Audit**: structured JSON log lines written to S3 (best-effort, no pipeline)
- **Deploy**: Managed EKS + Terraform from day one; GitHub Actions CI (test + build + push to ECR on every PR merge)
- **Observability**: four Grafana panels — query latency, connector fetch time, rate-limit rejections

**Exit**: End-to-end cross-app query works for 1 tenant with OPA-evaluated access control.

---

### M2 — Security Foundations

**Secrets secured. Audit durable. One enterprise SSO.**

Before any external pilot, two questions will be asked: "where are secrets stored?" and "can you show me an audit trail?" This month answers both.

- **KMS**: per-tenant data encryption keys using AWS KMS + envelope encryption. Secrets out of environment variables
- **Audit pipeline**: managed Kafka (MSK or Confluent Cloud) → managed S3 Sink Connector. Single topic `audit-events` with `tenant_id` as a field. Per-tenant S3 prefixes via `FieldAndTimeBasedPartitioner`. 60-second in-memory buffer before S3 flush. Offsets committed only after successful S3 write — at-least-once delivery, no data loss on failure
- **SSO**: one OIDC integration (Okta — most common in enterprise). Token-based HMAC auth deprecated
- **OPA still loads from disk**: control plane is M3. Policy change still means redeploy. Acceptable at 1–2 tenants
- **Freshness contract (minimal)**: every response returns `freshness_ms`, `freshness_source`, and connector rate-limit status so callers can distinguish live fetches from degraded behavior before shared caching arrives
- **Network security**: edge TLS and Kubernetes `NetworkPolicy` allow-lists arrive before full service-mesh rollout. In parallel, choose Istio and validate workload identity + mTLS on a narrow service path so east-west encryption is designed in early, not deferred to GA
- **Deploy**: still manual

**Exit**: Audit events in tenant-partitioned S3 within 60 seconds. Secrets in KMS. Okta SSO working. Responses expose freshness and rate-limit metadata.

---

### M3 — Control Plane Build

**Build the control plane service. YAMLs still running production.**

- **Control plane service** (Postgres-backed):
  - Tenant registry
  - Connector configs per tenant
  - Auth rules
  - Row filters + column masks
- **OPA bundle endpoint**: control plane exposes `GET /internal/bundles/policy.tar.gz`. Reads policies from Postgres, packages as OPA bundle format (tar.gz with `.rego` files + manifest). Built and tested — not yet the live policy source
- **3rd connector**: validates that the connector SDK generalizes across different auth flows and rate-limit models
- **Freshness + caching v1**: introduce an in-process source cache with `max_staleness`, soft TTL / hard TTL, and singleflight revalidation. Goal is quota protection and bounded staleness, not distributed scale yet
- **Rate-limit service design**: sketch interfaces — token bucket API, per-connector/tenant/user bucket hierarchy, Redis data structures, backpressure contract. No implementation; design artifact produced for M4 execution
- YAMLs remain the live config source while control plane is being validated

**Exit**: Control plane service running. Bundle endpoint tested. Freshness/caching v1 live. Redis rate-limit interfaces specced and reviewed. Data plane still on YAMLs.

---

### M4 — Control Plane Integration + Scale Primitives

**Cut over to control plane. Scale rate limiting and caching.**

- **YAML → control plane migration**: migrate all tenant configs, connector configs, and policies into Postgres. OPA switches to polling the control plane bundle endpoint (policy changes now take effect in ≤ 30s, no redeploy)
- **YAMLs fully deprecated** once all tenants migrated
- **Redis-backed rate limiting**: replaces in-memory, required when nodes scale beyond fixed infra
- **Redis-backed hot cache**: move the M3 source cache into Redis so hot fetches are shared across nodes
- **Async query path**: when a connector's rate budget is exhausted, queue the query and return a job ID for polling. Prevents blocking the request thread on slow upstreams
- **2nd OIDC integration**: pick based on early customer demand
- **Initial Istio rollout on core service paths**: enable service-to-service mTLS first for the query gateway, rate-limit service, and control-plane bundle path. Keep the scope narrow while the control-plane cutover and Redis scale work land

**Exit**: Control plane is the only config source. Policy change without redeploy. Rate limiting and Redis-backed hot cache operational across multiple nodes.

---

### M5 — OPAL + Materialization + Managed CD

**Sub-second policy revocation. Large joins handled. Deployment pipeline live.**

- **OPAL server**: connects to control plane Postgres, detects policy changes, pushes bundle updates to OPA via WebSocket. OPA evaluator code unchanged — only the bundle source switches from polling to push. Policy revocation propagates in < 2 seconds
- **S3-backed large-result materialization**: large source results and expensive joins spill to encrypted Parquet on S3 with short TTLs. Keeps the hot path simple while avoiding OOMs and repeated SaaS fetches
- **Managed CD pipeline**: GitHub Actions → ECR → AWS CodePipeline for staged environment promotion. EKS uses native rolling updates with readiness checks, health-based rollback, and a manual promotion gate for production
- **Performance testing**: Tech Lead drives. Identify bottlenecks under load. Fix before M6

**Exit**: Policy revocation < 2s. Large joins handled without OOM. Managed deployment promotion pipeline operational.

---

### M6 — GA Hardening

**Pen-test. Basic failure drills. Runbooks. Sign-off.**

- External pen-test and threat model review
- Basic failure drills (manual, not automated): kill a connector pod, kill a Redis node, stop the Kafka consumer — verify the system recovers and alerts fire. Not sophisticated chaos engineering; just confirming known failure paths behave as expected
- Runbooks for the failure modes covered in drills
- SLO dashboard with error budget tracking
- Fix performance regressions surfaced in M5 testing
- **Strict mTLS hardening**: expand Istio mTLS to all inter-service paths, enforce strict mesh policy in covered namespaces, and document break-glass / recovery procedures for sidecar or certificate failures
- Single region, multi-AZ. Rely on AWS for availability within region. Multi-region is out of scope for GA
- Last two weeks: perf fixes and sign-off process

**Exit (GA)**: Pen-test clean. Manual failure drills pass. 1k QPS sustained — P50 < 500ms, P95 < 1.5s, error rate < 0.5%. Runbooks written. Sign-off from TL, Security, PM, QA.

---

## What Gets Built When

| Capability | M1 | M2 | M3 | M4 | M5 | M6 |
|---|---|---|---|---|---|---|
| **Connectors** | GitHub + Jira (SDK established) | — | 3rd connector | 4th connector | — | — |
| **Query Planner** | SQL parse + fan-out + join | — | — | Async overflow (job ID) | S3-backed spill/materialization | — |
| **AuthN** | HMAC token | Okta OIDC | — | 2nd OIDC provider | — | — |
| **AuthZ — Evaluator** | OPA Go SDK embedded | — | — | — | — | — |
| **AuthZ — Policy Source** | Rego files on disk | Rego files on disk | Rego files on disk (bundle endpoint built, not live) | Bundle from control plane (30s poll, live) | OPAL push (< 2s) | — |
| **Circuit Breakers** | Per connector, from day one | — | — | — | — | — |
| **Rate Limiting** | In-memory + capped backoff/fail | — | — | Redis-backed, multi-node | — | — |
| **Caching** | — | — | In-process freshness cache (`max_staleness`, soft/hard TTL) | Redis-backed hot cache | S3 materialization / spill | — |
| **Audit** | Structured logs → S3 (best-effort) | Managed Kafka + S3 Sink (per-tenant prefix, 60s buffer, at-least-once) | — | — | — | — |
| **Secrets / KMS** | Env vars | Per-tenant KMS keys (envelope encryption) | — | — | Automated key rotation | — |
| **Control Plane** | YAML | YAML | Postgres-backed service built, YAMLs still live | Cutover complete, YAMLs deprecated | — | — |
| **CI** | GitHub Actions (test + build + push to ECR on PR) | — | — | — | — | — |
| **CD** | Manual deploy | Manual | Manual | Manual | Managed pipeline (CodePipeline) | — |
| **Infra** | Managed EKS + node groups, Terraform | MSK | Terraform modules expanded | Redis cluster | Multi-AZ | Chaos-validated |
| **Observability** | Grafana (4 panels) | — | Distributed tracing | — | Alerting + SLO budgets | Runbooks + manual failure drills |
| **Networking** | Edge TLS at gateway | Kubernetes `NetworkPolicy` allow-lists + Istio design / POC | — | Initial Istio mTLS on core service paths | — | Strict Istio mTLS enforcement + runbooks |

---

## OPA → OPAL Transition Detail

The policy evaluation path never changes. Only the bundle source evolves:

```
M1–M2:  [OPA Go SDK] ← .rego files on disk (bundled in container)
                         policy change = redeploy

M3–M4:  [OPA Go SDK] ← GET /internal/bundles/policy.tar.gz (30s poll)
                         policy change = DB update, picked up in ≤30s

M5+:    [OPA Go SDK] ← OPAL server pushes bundle on change
                         policy change propagates in <2s
```

The control plane bundle endpoint (`/internal/bundles/policy.tar.gz`) is ~200 lines in the control plane service: read policies from Postgres → serialize as `.rego` files → return as tar.gz with ETag. OPA handles polling, ETag-based caching, and retry natively.

---

## Risk Register

| Risk | Impact | Mitigation |
|---|---|---|
| **Connector variability** — every SaaS API has unique auth, pagination, rate limits, and schema quirks | High | Capability model in SDK abstracts differences; budget 2 weeks per new connector |
| **Schema drift** — SaaS APIs deprecate fields or change behavior without notice | Medium | Connector health checks detect drift; tenants can pin connector versions |
| **Hiring delays** — security or infra engineer unavailable by M2 | Medium | Core team covers basics; contract consultant for pen-test |

---

