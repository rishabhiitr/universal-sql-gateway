# Project Instructions

## Take-Home Project Brief (AUTHORITATIVE SOURCE - follow this exactly)

This is the complete, word-for-word take-home project brief. ALL design docs, execution plans, and prototype work MUST align with these requirements.

---

### Take-Home Project (V3, comprehensive): Universal SQL Across Enterprise Apps

#### Goal
Design and prototype a universal SQL query layer to query many SaaS applications used by enterprises. Assume scale to 100s of customers, 1000s of app types, and millions of users. Optimize for clarity of thinking, pragmatic trade-offs, and leadership-grade execution.

Target effort: ~6-10 hours. Prioritize depth where you make bold design choices.

#### Deliverables
1. **High-level design**: architecture, isolation, security/entitlements, freshness, rate limits, cost controls, and deployment modes (multi-tenant & single-tenant). Include diagrams.
2. **Six-month execution plan**: team shape, milestones, measurable acceptance criteria, risk register, and budget/infra assumptions.
3. **Prototype (focused scenario)**: one cross-app query end-to-end with auth, entitlement checks, rate-limit handling, and freshness control. Minimal CLI or HTTP API is fine.
4. **Readme**: quickstart + a short rationale documenting key trade-offs.

#### Functional Requirements
- Users can run SQL with projection, filters, pagination, and optional joins across external systems.
- Entitlements: enforce least-privilege access; row/column-level security (RLS/CLS) based on source permissions and tenant policy.
- Real-time: queries execute on demand on behalf of the user; support timeouts and partial results for slow sources.
- Rate limits: comply with per-app constraints; expose friendly, actionable messages when sync budgets are exhausted.
- Freshness: avoid materially stale data vs. sources; allow per-query staleness hints.
- Admin UX: admins can onboard connectors quickly via console or config; connectors are versioned.
- Deployment modes: multi-tenant and single-tenant supported without code changes.

#### Non-Functional Requirements
- Scale target: up to 10M users; peak ~1k QPS; ~100 MB/s data.
- Latency SLOs: P50 < 500 ms; P95 < 1.5 s for single-source predicate-pushdown queries.
- Availability SLO: Query Gateway 99.9% monthly; error budget policy.
- Autoscaling: horizontal scaling without manual intervention; cost guardrails.
- Rate-limit governance per connector, per tenant, per user; fairness across tenants.
- Freshness controls honoring rate limits; configurable per source/query class.
- Infra automation: Terraform modules; Helm/k8s; canary/blue-green with automated rollback.
- Security & isolation: storage/compute/network isolation; per-tenant keys; automated org off-boarding and crypto-shredding.
- Compliance signals: audit logs, access trails, data residency tags.

#### What to Submit
- Design doc (PDF/Markdown) with diagrams.
- Repo link (grant read to souvik-sen@ and careers@ema.co).
- Prototype quickstart (containerized or simple script) and 1-2 tests.
- One dashboard or trace screenshot and a short note on what it proves.

#### Choose Your Scenario (examples)
Pick one realistic cross-app query and implement end-to-end. Examples only - use equivalents if you prefer:
- Salesforce <-> Zendesk (accounts <-> tickets)
- GitHub <-> Jira (PRs <-> issues)
- Google Drive <-> Notion (docs by keyword in last 24h)

Minimal expectations: entitlement enforcement, rate-limit handling, and a freshness control. Keep UI minimal.

#### Reference & Hints
- For app categories and inspiration on connector surface areas, you may reference merge.dev/categories (no dependency required).
- Provide a short error vocabulary (e.g., RATE_LIMIT_EXHAUSTED, STALE_DATA, ENTITLEMENT_DENIED, SOURCE_TIMEOUT).
- Clearly document join strategy: federated on the fly vs. short-lived materialization.

---

### Architecture Expectations (comprehensive)

#### Core Concepts
- **Control Plane**: tenant/connector registry, schema catalog, policy store, secrets, rate-limit policies, audit.
- **Data Plane**: query gateway, planner, connector workers, caches/materialization, async job runners.
- **Isolation**: namespace per tenant; data encryption with tenant-scoped keys; per-tenant network boundaries (e.g., k8s namespaces), optional single-tenant clusters.

#### Components
- **Query Gateway**: AuthN via OIDC, AuthZ via policy (OPA or embedded engine), request shaping, timeouts; returns results + metadata (freshness_ms, rate_limit_status, trace_id).
- **Query Planner**: capability discovery, predicate/column pushdown, join plan, cost/freshness hints, spill to materialization when necessary.
- **Connector SDK**: capability model (tables/fields/ops/limits), auth/token refresh, pagination, concurrency contracts, standardized error codes.
- **Entitlement Service**: merges source permissions with tenant policies to compute RLS/CLS at plan time.
- **Rate-Limit Service**: token buckets/concurrency pools per connector/tenant/user; backoff and budget allocation; async overflow path.
- **Freshness Layer**: TTL caches, conditional requests (ETag/If-Modified-Since), incremental snapshots; per-source staleness contracts.
- **Materialization (optional)**: short-lived tables (DuckDB/ClickHouse/Parquet on S3) for joins/aggregations; lifecycle <= N minutes; encrypted per tenant.
- **Metadata Catalog**: Postgres for schemas/policies/tenants; migrations via Flyway or equivalent.
- **Secrets & Keys**: Vault + cloud KMS (tenant-scoped); rotation and break-glass.
- **Observability**: OpenTelemetry traces, Prometheus metrics, structured logs; exemplar dashboards and alerts.

#### SQL & Policy Surface (example, not prescriptive)
- Subset of SELECT (projection, WHERE, LIMIT, ORDER BY, optional JOIN).
- Policy DSL to express pre/post filters and masking rules (RLS/CLS). Document how policies are compiled into query plans.

#### Error & UX Model
- Standard error codes + human-readable messages; include Retry-After on rate-limit responses; guidance for switching to async.

#### Security & Compliance
- TLS everywhere; mTLS between services; per-tenant KMS keys; audit every cross-system access.
- Data residency tags drive storage and job placement; org off-boarding triggers crypto-shred and job cancellation.
- Threat model (STRIDE) + mitigations; pen-test readiness.

#### Capacity & Performance
- Sizing math for 1k QPS: concurrency limits, connector latency percentiles, cache hit ratios; head-of-line blocking avoidance.
- Autoscaling policies; backpressure; overload protection.

#### Deployment & Operations
- IaC: Terraform modules (networking, secrets, databases, clusters).
- CD: Helm, canary/blue-green, automatic rollback on SLO regression.
- DR/BCP: multi-AZ; optional multi-region active/active or active/passive; RPO/RTO targets.
- Runbooks: incident playbooks for rate-limit floods, connector auth failures, cache stampedes.

---

### Six-Month Execution Plan (example structure)
- **Team**: EM; 3 backend; 1 infra; 1 security; 1 QA; 0.5 PM; 0.5 DX.
- **M1**: Connector SDK v0; two connectors; entitlement model skeleton; SELECT/WHERE/LIMIT; rate-limit guard rails. Exit: demo with 1 tenant.
- **M2**: Planner with predicate pushdown; freshness TTL; per-tenant KMS; observability v1. Exit: P95 < 1.8s simple queries.
- **M3**: Policy DSL (RLS/CLS); async path + notifications; error vocabulary. Exit: clean UX under throttling.
- **M4**: Autoscaling; short-lived materialization; Helm/Terraform; DR basics. Exit: 1k QPS synthetic test.
- **M5**: Multi-tenant hardening; audit/alerts; perf tuning; cost guardrails. Exit: perf & cost report.
- **M6**: GA criteria; chaos drills; security review; onboarding playbook. Exit: readiness sign-off.
- Include risks (e.g., connector variability, quota exhaustion, schema drift) with mitigations.

---

### Prototype Requirements (detailed)
- **Interfaces**:
  - POST /v1/query with sql or plan JSON; returns rows + columns, freshness_ms, rate_limit_status, trace_id.
  - A minimal policy config expressing one RLS rule and one column mask.
- **Connectors**: 2 sources (real or mocked).
- **Entitlements**: user token -> scopes/roles -> RLS/CLS.
- **Rate-limits**: token bucket with burst; friendly error + optional async reroute.
- **Freshness**: max_staleness knob; show cache hit vs live fetch.
- **Testing**: small k6/Gatling script to reach ~500-1k QPS for 60s (local acceptable).
- **Observability**: one Prometheus metric and one trace that shows connector time.

---

### Submission Checklist
- Design doc + diagrams
- Repo link with access to souvik-sen@ and careers@ema.co
- Quickstart that runs locally; smoke test
- Screenshot of metrics/trace; short note

---

### Evaluation Rubric (weights)
- **Architecture & Trade-offs (30%)**
- **Security & Entitlements (15%)**
- **Freshness & Rate-Limits (15%)**
- **Execution Plan (15%)**
- **Prototype Quality (15%)**
- **Communication (10%)**

**Bonus**: cost levers, chaos plan, predicate pushdown creativity.

---

## Working Agreements

- **Scenario chosen**: GitHub <-> Jira (PRs <-> issues)
- **Language**: Go
- **Prototype is built** — see `docs/IMPLEMENTATION_PLAN.md` for the build order
- **Remaining deliverables**: Design Doc (`docs/DESIGN.md`), Execution Plan (`docs/EXECUTION_PLAN.md`), observability screenshots
