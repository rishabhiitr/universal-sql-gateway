# Security Design

This document covers the security architecture for the universal SQL query layer. It addresses transport security, data encryption, secrets management, tenant isolation, query-time data protection, audit, and tenant off-boarding.

---

## 1. Transport Security (mTLS)

All inter-service communication uses mutual TLS enforced by the Istio service mesh. Application code is unaware of TLS — it speaks plain HTTP to a local Envoy sidecar, which handles certificate provisioning, rotation, and mTLS handshake transparently.

- **Certificate authority**: Istio's control plane (istiod) issues short-lived SPIFFE-based workload identities (24h TTL) to each sidecar. No manual cert management.
- **Strict mode**: `PeerAuthentication` is set to `STRICT` cluster-wide — all plaintext connections between services are rejected.
- **Sidecar bypass prevention**: Defense in depth across four layers — kernel-level iptables redirect (istio-init), strict PeerAuthentication, K8s NetworkPolicies, and Pod Security Standards dropping `NET_ADMIN`/`NET_RAW` capabilities. No single layer is sufficient; together they make bypass effectively impossible without node-level compromise.
- **External TLS**: Client-to-gateway uses separately managed certificates (Let's Encrypt + cert-manager), rotated every 60 days.

| Layer | What It Prevents |
|---|---|
| iptables (istio-init) | Direct pod-to-pod plaintext |
| PeerAuthentication: STRICT | Bypassing encryption |
| K8s NetworkPolicy | Unauthorized pod access |
| Pod Security Standards | Container breakout, iptables tampering |

---

## 2. Encryption at Rest

All persistent and cached data is encrypted using **envelope encryption** with per-tenant keys.

### Pattern

1. Call `KMS.GenerateDataKey(tenant_CMK)` — returns `(plaintext_DEK, encrypted_DEK)`.
2. Encrypt data locally with `plaintext_DEK`; immediately discard plaintext from memory.
3. Store `[encrypted_data || encrypted_DEK]` together.
4. On read, call `KMS.Decrypt(encrypted_DEK)` to recover the plaintext DEK, decrypt locally.

**Why envelope, not direct KMS encryption?** KMS has throughput limits (~30K req/s per region) and adds network latency per call. Envelope encryption reduces KMS calls to one per data key, not one per row.

### Per-Tenant Key Isolation

Each tenant gets a dedicated KMS Customer Managed Key (CMK). Benefits:
- Compromise of one tenant's CMK does not affect others.
- Enables crypto-shredding on off-boarding (delete CMK → all wrapped DEKs become permanently undecryptable).
- Per-tenant key usage appears in CloudTrail for audit.
- Cost: ~$1/CMK/month — negligible at 100s of tenants.
- Naming: `alias/queryfed/{tenant_id}/{env}`

### In-Process DEK Cache

To reduce KMS round-trips on read, cache plaintext DEKs in an in-process LRU (via AWS Encryption SDK Caching CMM):

| Parameter | Value | Purpose |
|---|---|---|
| `max_age` | 5-15 min | Bound DEK reuse window |
| `max_messages_encrypted` | 100-1,000 | Limit reuse count |
| `max_bytes_encrypted` | 100 MB | Limit data volume per DEK |

Cache key is always `(tenant_id, data_classification)` — never shared across tenants.

### DEK Lifecycle & Durability

KMS generates DEKs on demand but does **not** store them. The application owns DEK persistence:

| Data Type | DEK Storage | Loss Impact |
|---|---|---|
| Query Cache (Redis) | Alongside Redis entry | Acceptable — re-fetch from source |
| Materialization Store | Postgres metadata table | Recoverable — re-materialize |
| Audit Logs (S3) | DynamoDB (separate from S3) | Catastrophic — permanent loss |

Audit log DEKs go in DynamoDB (point-in-time recovery, conditional writes) rather than S3 metadata, which can be overwritten.

---

## 3. Secrets Management

### Vault vs. KMS: Two Tools, Two Jobs

| Secret Type | Tool | Rationale |
|---|---|---|
| Connector credentials (API keys, OAuth tokens) | HashiCorp Vault | Lifecycle management: leasing, TTL, rotation, revocation |
| Encryption master keys (CMKs) | AWS KMS | Keys never leave HSM; IAM-integrated access control |

Vault manages secrets that applications consume. KMS manages cryptographic keys that protect data at rest. Not interchangeable.

### Data Plane Access: Vault Agent Sidecar

The data plane must not call the control plane at query time for credentials. This would introduce coupling, latency, and a single point of failure.

- Vault Agent sidecar authenticates once at startup (K8s service account JWT), fetches tenant connector credentials.
- Credentials mounted as files at `/vault/secrets/` — query executor reads from local filesystem, zero network calls to Vault at runtime.
- Sidecar watches lease TTLs and refreshes credentials in the background before expiry.
- Access boundary: data plane holds read-only Vault policy scoped to `secret/tenants/*`; control plane holds write policy.

**Why sidecar, not embedded client?** Vault Agent meets the three sidecar criteria: (1) cross-cutting concern identical across services, (2) stable file-on-disk interface, (3) off-the-shelf maintained binary. A separate process also means a token renewal bug doesn't crash query serving.

### Control Plane / Data Plane Secret Boundary

The interface between planes for secrets is **Vault itself**, not a REST API:

| Phase | Actor | Action |
|---|---|---|
| Provisioning | Control Plane | OAuth flow → writes token to Vault |
| Query execution | Data Plane | Reads from Vault Agent's local mount |
| Token rotation | Control Plane | Detects expiry → writes new version → sidecar propagates |

Anti-pattern: data plane calling `GET /internal/credentials/{tenant_id}` on a control plane endpoint.

---

## 4. Tenant Isolation — Tiered Model

### Why Not Namespace-Per-Tenant for Everyone?

K8s supports ~10K namespaces per cluster, but each creates etcd objects (ResourceQuotas, NetworkPolicies, RBAC). At 1000s of tenants, etcd watch pressure and API server latency increase. More importantly, our pods are **not tenant-specific** — all tenants flow through the same gateway/planner/connector pods. A per-tenant namespace would just run identical service copies.

### Three Tiers

| Tier | Isolation Level | Target | When |
|---|---|---|---|
| **1 — Shared Infrastructure** | Logical + cryptographic | Majority (~90%+) | M1+ |
| **2 — Dedicated Namespace** | Namespace + resource quotas | Enterprise / compliance | M4 |
| **3 — Dedicated Cluster** | Full physical isolation | Regulated (PCI-DSS, HIPAA) | M6 |

### Tier 1 — Shared Infrastructure (Default)

Tenants share namespaces grouped by region/shard (e.g., `data-plane-us-east-1`). Isolation through:

**Cryptographic isolation**: Per-tenant KMS CMK, envelope encryption on all cached/materialized data, crypto-shredding on off-boarding.

**Application-level isolation**: Tenant ID from JWT enforced in middleware; per-tenant rate limits; RLS/CLS scoped to tenant policies; cache keys always include `tenant_id`.

**Network isolation (pod-label NetworkPolicies)**: In a shared namespace, NetworkPolicies enforce *service topology* — only gateway pods can reach connector pods. Tenant isolation at the network layer doesn't need per-tenant rules because there are no tenant-specific pods; the per-tenant boundary is JWT → entitlements → encryption.

```yaml
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: connector-ingress
  namespace: data-plane
spec:
  podSelector:
    matchLabels:
      role: connector-worker
  policyTypes: [Ingress]
  ingress:
    - from:
        - podSelector:
            matchLabels:
              role: gateway
```

**Two independent network enforcement points**:

| | K8s NetworkPolicy | Istio AuthorizationPolicy |
|---|---|---|
| Enforced by | CNI plugin (kernel, iptables/eBPF) | Envoy sidecar (userspace) |
| Layer | L3/L4 (IP, port) | L7 (identity, path, headers) |
| Survives sidecar bypass? | Yes | No |
| Can verify caller identity? | No | Yes (SPIFFE via mTLS certs) |

These don't "understand" each other — defense in depth. NetworkPolicy stops traffic at the kernel even if Istio is bypassed. Istio stops traffic from the right IP but wrong service identity.

**Per-tenant concurrency control**: `semaphore.Weighted` (from `golang.org/x/sync/semaphore`) keyed by `(tenant, connector)` — lightweight integer counter that gates concurrent in-flight requests per tenant per connector. No per-tenant goroutine pools needed.

### Tier 2 — Dedicated Namespace (Enterprise)

For tenants requiring stronger resource isolation (contractual guarantees, noisy-neighbor protection):
- Separate K8s namespace with ResourceQuota and LimitRange
- Dedicated pod replicas for gateway and connectors
- Namespace-scoped NetworkPolicies (deny-all default)
- Expected scale: ~100s of namespaces, well within K8s limits

### Tier 3 — Dedicated Cluster (Regulated)

For PCI-DSS, HIPAA, or data sovereignty requirements:
- Fully separate EKS/GKE cluster with dedicated Redis, Postgres, S3
- Same Helm chart with `deployment.mode=single-tenant` values
- Terraform module provisions the entire stack
- No shared state with any other tenant

---

## 5. Data Protection During Query Execution

### Data States

| State | Encrypted? | Key |
|---|---|---|
| In transit (between services) | Yes — mTLS | Istio-managed certs |
| In cache (Redis) | Yes — envelope encryption | Tenant DEK → tenant CMK |
| At rest (S3, Postgres) | Yes — envelope encryption | Tenant DEK → tenant CMK |
| In memory (processing) | No — plaintext required | N/A |
| Temp files (materialization spill) | Infra-level only (EBS/node encryption) | AWS-managed key |

### Temp File Protection

Per-tenant KMS encryption on short-lived spill files adds latency for marginal benefit — files exist for seconds to minutes and are deleted after query completion. Instead, we use access controls and ephemeral storage:

- **RAM-backed tmpfs** for small materializations (`emptyDir` with `medium: Memory`, `sizeLimit: 512Mi`)
- **Disk-backed emptyDir** for larger joins (cleaned on pod restart)
- Filesystem permissions `0700` — only the connector worker process can read
- Pod Security Standards: read-only root filesystem, `drop: ALL` capabilities
- Node-level EBS encryption provides the base layer
- Ephemeral lifecycle: auto-cleaned after query; pod restart wipes everything

**Trade-off**: Hot-path uses OS/infra-level encryption + strict access controls rather than per-tenant encryption on temp files. Standard industry practice — the threat model for temp files is node-level compromise, which defeats any in-node encryption anyway.

---

## 6. Audit & Compliance

- **Every cross-system access logged**: who, what, when, tenant, connector, trace_id.
- **Immutable audit store**: S3 with object lock + CloudTrail integration.
- **Separate audit CMK**: Audit logs use a dedicated KMS key, distinct from cache/materialization CMKs. A misconfiguration in one domain doesn't affect audit integrity.
- **Audit key governance**: Never delete audit CMKs; use `DisableKey` for retirement. 30-day deletion waiting period (max). Encrypted DEKs in DynamoDB with point-in-time recovery, 7+ year retention (SOC 2, HIPAA). Annual KMS key rotation (prior versions retained).
- **Data residency tags**: Tenant config drives storage and job placement by region.
- **Compliance signals**: Access trails, data residency tagging, org off-boarding triggers.

---

## 7. Off-boarding & Crypto-shredding

Tenant deletion triggers cleanup across both planes:

**Data plane (crypto-shred)** — encrypted data made unrecoverable by destroying key material:
1. **Disable KMS CMK** — cache, materialization, and audit data becomes immediately unreadable.
2. **Purge cache** — Redis entries for tenant deleted (prefix delete).
3. **Delete materialized data** — S3 objects and temp files removed.
4. **Schedule CMK deletion** — 30-day waiting period, then permanent.

**Control plane (hard delete)** — metadata not encrypted with tenant CMK, surgically removed:
5. **Delete tenant metadata** — Postgres rows: tenant config, connector registry, policies, schemas.
6. **Purge Vault secrets** — connector credentials revoked and deleted.
7. **Tier 2/3 cleanup** — dedicated namespace deleted / dedicated cluster Terraform-destroyed.

---

## 8. STRIDE Threat Model

| Category | Primary Targets | Mitigations |
|---|---|---|
| **Spoofing** | User identity, service identity | OIDC/JWT validation at gateway; mTLS with SPIFFE identities between services |
| **Tampering** | Query parameters, cached data, audit logs | Signed JWTs; envelope encryption on cache; S3 object lock on audit |
| **Repudiation** | Cross-system data access | Immutable audit log with tenant/user/connector/trace_id per access |
| **Information Disclosure** | Tenant data leakage, credential exposure | Per-tenant KMS keys; Vault with scoped policies; RLS/CLS post-fetch; no cross-tenant cache keys |
| **Denial of Service** | Rate-limit exhaustion, noisy tenant | Per-tenant token buckets; semaphore-based concurrency limits; autoscaling; backpressure |
| **Elevation of Privilege** | Container breakout, RBAC bypass | Pod Security Standards (drop ALL); read-only root FS; namespace RBAC; distroless base images |

---

## 9. Pen-test Readiness

- **Hardened containers**: Distroless base images, non-root execution, read-only root filesystem.
- **Dependency scanning**: Snyk/Dependabot in CI pipeline.
- **Cluster RBAC**: Least-privilege roles; no cluster-admin for application service accounts.
- **Network segmentation**: Default-deny NetworkPolicies; explicit allow-list per service role.
- **Secret hygiene**: No secrets in env vars or container images; Vault-managed with short TTLs.
