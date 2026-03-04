# Control Plane Design Notes

## Architecture Decision: Single Service, Single Database

The control plane is a single Go service backed by one Postgres database. All control plane operations are low-throughput CRUD on configuration data — there is no reason to split into microservices.

**Why one service is correct:**
- All operations are low throughput (admin CRUD, not on the query hot path)
- Read-heavy: data plane pulls config periodically, admins update it occasionally
- Relationally connected: disabling a connector for a tenant should also clean up its policies and rate-limit configs — one Postgres transaction
- At 100s of tenants and low-frequency config changes, a single service handles this trivially

**What lives outside Postgres but is logically control plane:**
- **Vault** — secrets and encryption keys (separate deployed service)
- **S3 with object lock** — audit log WORM storage
- **OPA + OPAL** (production) — policy evaluation engine when policies grow complex

---

## Control Plane Components

### Postgres Tables

| Table | What It Stores | Keyed On |
|---|---|---|
| `tenants` | Tenant registry (id, status, region, created_at) | `tenant_id` |
| `tenant_connectors` | Which connectors enabled per tenant, version, config overrides | `(tenant_id, connector_id)` |
| `rate_limit_configs` | Per (tenant, connector) rate limits (req/min, burst) | `(tenant_id, connector_id)` |
| `entitlement_policies` | RLS/CLS rules — allowed roles, row filters, column masks | `(tenant_id, table)` |
| `schema_catalog` | Tables, columns, types, connector capability declarations | `(connector_id, table)` |

### Connector Versioning & Onboarding

Connectors are versioned (`major.minor.patch`). The `tenant_connectors` table stores the active version per tenant. Admins onboard connectors via YAML config or the Admin Console:

```yaml
tenant: acme-corp
connectors:
  - id: github
    version: "1.3.0"
    credentials_vault_path: "secret/acme/github"
    rate_limit_override:
      requests_per_minute: 500   # acme has a GitHub Enterprise plan
  - id: jira
    version: "2.1.0"
    credentials_vault_path: "secret/acme/jira"
```

Version upgrades are non-breaking by default. Breaking changes (schema renames, removed fields) require a major version bump and a migration path in the connector changelog.

### External Stores

| Store | Purpose | Why Not Postgres |
|---|---|---|
| Vault + Cloud KMS | OAuth tokens, API keys, refresh tokens, per-tenant encryption keys | Purpose-built for secrets; rotation, break-glass, audit; never roll your own |
| S3 (object lock) | Audit log WORM storage | Immutable, cheap, regulatory retention (1-7 years); Postgres isn't designed for append-only WORM |

### HLA

```
┌─── Control Plane ────────────────────────────────────────────────┐
│                                                                   │
│   ┌──────────────────────────────┐                                │
│   │    Control Plane API         │  ← single Go service           │
│   │                              │                                │
│   │  /tenants                    │                                │
│   │  /connectors                 │                                │
│   │  /policies                   │                                │
│   │  /schemas                    │                                │
│   │  /rate-limits                │                                │
│   └──────────┬───────────────────┘                                │
│              │                                                    │
│   ┌──────────▼──────────┐  ┌────────┐                            │
│   │  Postgres            │  │ Vault  │                            │
│   │                      │  │        │                            │
│   │  tenants             │  │secrets │                            │
│   │  tenant_connectors   │  │keys    │                            │
│   │  rate_limit_configs  │  └────────┘                            │
│   │  entitlement_policies│                                        │
│   │  schema_catalog      │                                        │
│   └──────────────────────┘                                        │
│                                                                   │
│   ┌──────────────────────────────────────────────────────────┐   │
│   │  Audit Pipeline (separate deploy, same repo)              │   │
│   │  Kafka Connect S3 Sink → S3 WORM (Parquet, per tenant)   │   │
│   │  Query via Athena (serverless, zero infra)                │   │
│   └──────────────────────────────────────────────────────────┘   │
└───────────────────────────────────────────────────────────────────┘
```

---

## Data Plane ↔ Control Plane Interaction

### Principle: Data Plane Caches Locally, Control Plane is Source of Truth

The data plane caches control plane data locally with short TTLs. This eliminates synchronous control plane calls on the query hot path.

**Why the data plane owns the caching (not the control plane):**
- At 1k QPS, every query needs policy, schema, and rate-limit config. Synchronous gRPC to the control plane would add 3-9ms overhead per query.
- If the control plane goes down, cached data lets the data plane continue serving queries (availability decoupling).
- The control plane stays a low-QPS config service; it doesn't need to scale with query traffic.

**Analogy**: DNS. The authoritative nameserver (control plane) doesn't cache. The resolver (data plane) caches with a TTL. The resolver is the one serving high-QPS traffic.

### What the Data Plane Caches

| Data | Cached Where | TTL | Refresh Mechanism |
|---|---|---|---|
| Entitlement policies (RLS/CLS) | In-memory Go struct | 30-60s | gRPC pull (baseline), OPAL push (production) |
| Schema catalog + capabilities | In-memory Go struct | 30-60s | gRPC pull |
| Rate-limit configs | In-memory; live token buckets are data plane state | 30-60s | gRPC pull; event bus push for decreases |
| Connector registry (enabled/disabled) | In-memory | 30-60s | gRPC pull; event bus push for disables |
| Credentials (OAuth tokens, API keys) | In-memory in connector | Until expiry or 401 | See "Credential Lifecycle" section below |

---

## Staleness Inventory: What Happens When Cached Data is Stale

### Tiered by Severity

**High severity (security boundary):**

| Change | Risk | Mitigation | Propagation |
|---|---|---|---|
| Entitlement revocation / policy change | Revoked user can still query for TTL window | OPA + OPAL push (~1-2s) | Push |
| Tenant off-boarding | Queries still served for deactivated tenant | Event bus + hard block (immediate) | Push |

**Medium severity (operational):**

| Change | Risk | Mitigation | Propagation |
|---|---|---|---|
| Rate limit **decrease** | Old higher limit burns SaaS API quota | Event bus push (~1-2s) | Push |
| Connector disabled for tenant | Queries succeed against disabled connector | Event bus push (~1-2s) | Push |

**Low severity (TTL is sufficient):**

| Change | Risk | Mitigation | Propagation |
|---|---|---|---|
| Rate limit increase | Tenant can't use higher quota for 60s | No harm; fails conservatively | TTL pull |
| New connector enabled | Queries fail with "not found" for 60s | Fails closed; safe | TTL pull |
| Schema / capability change | Suboptimal query plan for 60s | Connector errors handled gracefully | TTL pull |

**Residual risk accepted**: For push events, ~1-2s propagation window remains. Mitigated by: (a) audit log captures all access including during the window, (b) synchronous policy check available as per-tenant toggle for zero-window revocation customers.

---

## Entitlement / Authorization Evolution

The auth system progresses through 4 stages. Each earns its complexity.

### Step 1 — Prototype / MVP (Month 1)
- Gateway validates JWT (HMAC-SHA256)
- `policy.yaml` loaded from disk at startup into a Go struct
- Embedded `entitlements.Engine` evaluates RLS/CLS in-process
- Zero external dependencies for auth

### Step 2 — Policies move to Postgres (Month 2-3)
- Same embedded engine, same evaluation logic
- Policy source changes from YAML file to Postgres table
- Admin API to create/update policies per tenant without redeploying
- Data plane loads policies at startup + refreshes on 30-60s TTL
- JWT validation moves to OIDC (verify against IdP JWKS endpoint)

### Step 3 — OPA SDK Embedded (Month 3-4)
- Replace custom Go policy engine with OPA's Go SDK (`github.com/open-policy-agent/opa/sdk`)
- Policies written in Rego instead of custom YAML DSL
- Still evaluated **in-process** — no sidecar, no network call
- Why: Rego handles complex conditional policies (time-based, attribute-based, team-scoped). Security teams know Rego.
- Key: Rego policies written here don't change when moving to Step 4.

### Step 4 — OPA Sidecar + OPAL (Month 5-6)
- OPA moves from embedded SDK to sidecar (or daemonset) per data plane pod
- OPAL server added to control plane — watches policy store, pushes deltas to all OPA instances
- Push-based invalidation for access revocations (~1-2s)
- This is the "regulated enterprise with 50+ tenants and strict revocation SLAs" stage

#### How OPAL Propagation Works

```
Admin revokes access (Postgres write)
        │
        ▼
   OPAL Server (Control Plane)
   — watches entitlement_policies table via CDC/polling
   — detects policy delta
        │
        ▼  pushes delta to ALL OPA sidecars (~1-2s)
        │
   ┌────▼────────────────────────────────┐
   │  OPA Sidecar (per data-plane pod)   │
   │  — holds full bundle in-memory      │
   │  — patches the delta in-place       │
   └────────────────┬────────────────────┘
                    │  local HTTP (loopback)
                    ▼
          Query Gateway (main process)
          POST http://localhost:8181/v1/data/authz/allow
          input: {user, tenant, table}
          → {"result": true}  or  {"result": false}
```

OPA is queried **per-request at plan time** for a binary table allow/deny. It is not involved in column masking or row filtering — those are applied by the executor post-fetch (see data-plane-design.md).

#### Bundle Structure — Lean Bundle (Rules Only)

The OPA bundle contains **only Rego policy files**. It never contains user rosters, role assignments, or row-level data.

- **In bundle**: Rego rules per tenant (e.g., `data.authz.allow` checks `input.user.roles`)
- **Not in bundle**: user→role mappings, tenant→user lists, row-level data
- **At evaluation time**: user context (`{user_id, tenant_id, roles}`) is passed as `input` — injected per request from the validated JWT, not loaded into the bundle

**Why**: Loading user-level data into the bundle would scale with user count (10M users × role assignments = GBs). Rules stay small regardless of user count; context is injected per request.

**Key invariant**: If someone proposes adding a user roster or permission table to the OPAL bundle, reject it. That data belongs in Postgres, queried at plan time, passed as `input`.

#### OPA Sidecar Resource Sizing

| Parameter | Value | Rationale |
|---|---|---|
| Memory limit | 256 MB | Lean bundle (rules only); 100s of tenants × small Rego files |
| CPU limit | 200m | Policy evaluation is CPU-light at 1k QPS |
| Max bundle size | 50 MB compressed | OPAL rejects oversized bundles before pushing |
| Bundle reload | Delta patching | OPAL pushes only changed tenant policies, not the full bundle |

### When to move to the next step

| Step | Trigger | Gain |
|---|---|---|
| 1 → 2 | Second tenant onboards with different policies | Per-tenant policies without redeploy |
| 2 → 3 | Policy rules get conditional/complex, or security audit | Standard policy language (Rego), composable rules |
| 3 → 4 | Enough data plane nodes that TTL staleness is a compliance concern | Real-time policy propagation, sub-second revocation |

**Honest assessment**: Most startups never get past Step 2. OPA SDK is worth it if policy complexity grows. OPAL is worth it for enterprises with strict revocation SLAs (SOC2 Type II, FedRAMP).

---

## Credential / Secrets Lifecycle

Credentials for SaaS APIs (OAuth tokens, PATs, API keys) are stored in Vault, cached in-memory in the connector, and refreshed on expiry or rejection.

### Flow

```
                  ┌─────────────────────────────┐
                  │         Vault                │
                  │  secret/acme/github          │
                  │    access_token: "gho_abc"   │
                  │    refresh_token: "ghr_xyz"  │
                  │    expires_at: 1709251200    │
                  └──────────────┬──────────────┘
                                 │
                   Fetched ONCE, then cached in-memory
                   Re-fetched only on expiry or 401
                                 │
                                 ▼
                  ┌─────────────────────────────┐
                  │  Connector Credential Cache  │
                  │  (in-memory, per data plane) │
                  │  ("acme","github") → {token} │
                  └─────────────────────────────┘
```

### Three Scenarios

**Happy path (99% of requests):**
Token in cache, not expired → use it → SaaS returns 200 → done. Zero Vault calls.

**Token expired (proactive refresh, every ~30-60 min for OAuth2):**
1. Token in cache but `expires_at < now`
2. Fetch `refresh_token` from Vault (synchronous read)
3. Call SaaS token endpoint with refresh token → get new access token
4. Store new tokens back in Vault (synchronous write)
5. Update in-memory cache
6. Make API call with new token

Adds ~20-50ms to ONE request every 30-60 minutes. Not on the hot path.

**Token revoked externally (reactive, rare):**
1. Token in cache, not expired (we think it's valid)
2. Call SaaS API → 401
3. Discard cached token
4. Fetch fresh credentials from Vault
5. Retry with new token
6. If still 401 → refresh token also dead → return `ENTITLEMENT_DENIED`

### Why credentials don't need push-based invalidation

| | Policy / Rate-limit config | Credentials |
|---|---|---|
| Staleness detectable? | No — stale policy silently gives wrong answer | Yes — 401 from SaaS, self-heals |
| Invalidation model | TTL or push (OPAL, event bus) | Reactive (401) or proactive (check expires_at) |
| Risk of staleness | Security (wrong access decision) | One failed request, then self-heals |

The SaaS API tells you when credentials are stale. Policies don't.

---

## Rate Limit Architecture

Control-plane view only:

- **DB schema**:
  - `rate_limit_configs` stores per `(tenant_id, connector_id)` rate-limit config and overrides.
  - `schema_catalog` stores connector capability metadata, including limiter model declarations.
- **Budget domains hosted by policy**:
  - SaaS provider budget (external constraint; enforced by source 429s)
  - Platform shared budget (internal governance across tenants)
  - Tenant connector allocation (internal fairness and quota assignment)
- **Ownership split**: control plane stores policy/config; data plane owns live limiter state and runtime enforcement.

For detailed models, enforcement levels, limiter types, cache interaction, async overflow, and production evolution, see [`docs/rate-limit-design-notes.md`](./rate-limit-design-notes.md).

---

## Audit Pipeline

### Architecture

```
Data Plane           Kafka              Enrichment (opt)     S3
(producer)           (durable buffer)   (Flink/Streams)      (compliance archive)

┌──────────┐        ┌──────────────┐   ┌──────────────┐    ┌────────────┐
│  Query   │──────→ │ audit.events │──→│ Normalize,   │───→│ S3 (WORM)  │
│  Gateway │ async  │ .raw         │   │ enrich,      │    │ Parquet    │
│          │        │ partitioned  │   │ tag          │    │ per tenant │
└──────────┘        │ by tenant_id │   └──────────────┘    │ /date      │
                    └──────────────┘                        └────────────┘
```

### Design Decisions

**Async produce from data plane**: Audit event production must not block the query response. The gateway produces to Kafka asynchronously after returning the response. Confluent and Conduktor both recommend this: "Synchronous audit logging that blocks operations can introduce unacceptable latency."

**Kafka as durable buffer**: At 1k QPS with ~2 audit events per query, that's ~2k events/sec, ~2-4 MB/s. Kafka provides durability (events survive pod crashes), ordering per partition (tenant-level ordering for trail reconstruction), and replay capability (if S3 write fails, replay from Kafka).

**Partitioned by `tenant_id`**: Gives ordering per tenant, makes per-tenant deletion straightforward for off-boarding, and aligns with how compliance queries are scoped.

**Encrypt at the producer**: Audit event encrypted with tenant's KMS key before writing to Kafka. Data is encrypted in Kafka and in S3. Kafka brokers and the S3 writer never see plaintext.

**Kafka Connect S3 Sink (consumer)**: No custom code needed. Batches events, writes Parquet to S3 partitioned by `tenant_id/date/`. S3 object lock for WORM. Lives as a separate deployable in the control plane repo — same team owns compliance, audit policy, and querying.

**Optional enrichment layer**: Flink or Kafka Streams between raw and processed topics. Resolves `user_id` → name/team, adds geo from source IP, tags with compliance labels (PII, HIPAA). Makes audit data human-readable for compliance officers.

### What to audit

Each query generates one post-completion event containing:
- `trace_id`, `timestamp`
- `tenant_id`, `user_id`, `roles`
- SQL executed, tables accessed, connectors called
- Row count returned, cache hit/miss per source
- Entitlement decisions (denied tables, masked columns)
- Rate limit status, latency
- Error code (if any)

For belt-and-suspenders (production): emit a pre-query event before execution starts, correlated by `trace_id`. Guarantees audit record even if pod crashes mid-execution.

### Retention

| Regulation | Minimum Retention |
|---|---|
| SOC 2 | 1 year |
| HIPAA | 6 years |
| SOX | 7 years |
| GDPR | Varies; must support per-user deletion |

S3 lifecycle policies handle tiering (Standard → IA → Glacier) based on age.

---
