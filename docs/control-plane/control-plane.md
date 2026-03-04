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
| `tenant_connectors` | Which connectors enabled per tenant, credentials path, config overrides | `(tenant_id, connector_id)` |
| `rate_limit_configs` | Per (tenant, connector) rate limits (req/min, burst) | `(tenant_id, connector_id)` |
| `entitlement_policies` | RLS/CLS rules — allowed roles, row filters, column masks | `(tenant_id, table)` |
| `schema_catalog` | Tables, columns, types, connector capability declarations | `(connector_id, table)` |

### Connectors: Development, Deployment, and Tenant Enablement

Three distinct concerns — owned by different people, on different timelines.

#### 1. Connector Development (engineering)

A connector is Go code that translates SQL operations into SaaS API calls. Each connector implements the `Connector` interface (see [`docs/data-plane/connector.md`](../data-plane/connector.md)) — schema discovery, predicate pushdown, pagination, auth token injection, error mapping. This is engineering work: code review, tests, CI. A new connector for a SaaS app doesn't exist until someone writes and ships this translation layer.

The `schema_catalog` table stores the connector's capability declarations (supported tables, columns, filterable fields, pagination model) — populated by the connector's `Capabilities()` method, not by admin config.

#### 2. Connector Deployment (platform/infra)

Connectors run in-process with the data plane gateway. Deploying a new connector version means deploying a new gateway binary. Versioning (`major.minor.patch`), canary rollout, and rollback are handled by the deployment pipeline (Helm, k8s rolling updates) — not by tenant config. All tenants on a given data plane instance run the same connector code version.

Breaking changes (schema renames, removed fields) require a major version bump, a migration path, and coordination with affected tenants before rollout.

#### 3. Tenant Enablement (admin config)

Once a connector is deployed, admins enable it for a tenant. This is the config operation — stored in `tenant_connectors`:

```yaml
tenant: acme-corp
connectors:
  - id: github
    credentials_vault_path: "secret/acme/github"
    rate_limit_override:
      requests_per_minute: 500   # acme has a GitHub Enterprise plan
  - id: jira
    credentials_vault_path: "secret/acme/jira"
```

This config controls: which connectors the tenant can query, where their credentials live in Vault, and any rate-limit overrides tied to their SaaS plan tier. 

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

## Entitlement / Authorization

### Why OPA + OPAL

The core authorization decision is table-level gating: can this `(user_role, tenant)` pair query this table? At scale, enterprise customers will need richer rules — time-based access, team-scoped visibility, conditional policies that combine multiple attributes. This is ABAC territory.

**OPA** (Open Policy Agent) is the right fit. Policies written in Rego — a declarative language purpose-built for authorization. Evaluated in-process via the Go SDK (`opa.Eval()`, sub-millisecond, no network call). CNCF graduated and battle-tested (Netflix, Goldman Sachs, Atlassian). The complexity delta over a custom Go function is small, and Rego handles rich ABAC rules without rewriting the auth layer as policy needs grow.

**OPAL** (Open Policy Administration Layer) solves policy distribution. When an admin revokes access in Postgres, OPAL detects the change and pushes the updated policy bundle to all OPA instances within ~1-2 seconds. Without OPAL, policy changes propagate only on TTL-based polling (30-60s) — unacceptable for access revocations at enterprise scale.

The evolution from embedded OPA SDK (M1) to OPA + OPAL push (M5) is covered in [`docs/EXECUTION_PLAN.md`](../EXECUTION_PLAN.md). The evaluator code and Rego policies never change — only the bundle delivery mechanism evolves.

### How OPAL Propagation Works

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

OPA is queried **per-request at plan time** for a binary table allow/deny. The input is the JWT claims (`user_id`, `tenant_id`, `roles`) — no runtime enrichment, no extra data fetching. Column masking and row filtering are not OPA's concern — those are applied by the executor post-fetch (see [`docs/data-plane/executor.md`](../data-plane/executor.md)).

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
