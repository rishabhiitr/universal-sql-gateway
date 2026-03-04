# Security Design Notes

## mTLS via Service Mesh (Istio)

### Core Question: How does mTLS work with service mesh?

**Key Insight**: Individual microservices don't manage or trust each other's certificates. The service mesh handles everything.

### Architecture

```
Service A  <-->  Envoy Proxy A  <==mTLS==>  Envoy Proxy B  <-->  Service B
(plain HTTP)     (sidecar)                   (sidecar)           (plain HTTP)
```

#### Without Service Mesh (Manual mTLS)
Each service needs to:
- Have its own TLS cert + private key
- Know and trust every other service's CA/cert
- Handle cert rotation, revocation
- Implement TLS handshake in application code

**Problem**: Nightmare at scale with hundreds of services.

#### With Istio/Linkerd (Sidecar-based mTLS)
- **Istio's control plane (istiod)** acts as the CA — issues short-lived certs (SPIFFE identity) to each sidecar automatically
- **Envoy sidecars** handle all mTLS — cert provisioning, rotation, handshake, verification
- **Application code** speaks plain HTTP to `localhost` — zero TLS awareness
- **Trust model**: Centrally managed by Istio's CA, not peer-to-peer between services

### Certificate Trust Model

**Services do NOT trust each other's certs directly.** They don't even see them.

- All Envoy sidecars trust the **same root CA** (Istio's built-in CA or external CA like Vault/cert-manager)
- Istio control plane issues short-lived workload certificates (default: 24h TTL)
- Automatic rotation before expiry
- SPIFFE-based identities (e.g., `spiffe://cluster.local/ns/default/sa/query-gateway`)

### Preventing Sidecar Bypass

**Question**: What stops someone from bypassing the sidecar and hitting a service directly?

**Answer**: Multiple defense layers.

#### Layer 1: iptables Rules (Primary Enforcement)

When Istio injects the sidecar, it runs an **init container (`istio-init`)** that sets up iptables rules in the pod's network namespace:

```
Incoming traffic → iptables REDIRECT → Envoy sidecar (port 15006) → app (port 8080)
Outgoing traffic → iptables REDIRECT → Envoy sidecar (port 15001) → destination
```

**Effect**: Even if someone tries to hit port 8080 directly from within the cluster, kernel-level iptables rules intercept and force it through Envoy first. The app literally cannot receive traffic that didn't pass through the sidecar.

#### Layer 2: PeerAuthentication Policy

```yaml
apiVersion: security.istio.io/v1beta1
kind: PeerAuthentication
metadata:
  name: default
  namespace: istio-system
spec:
  mtls:
    mode: STRICT  # Reject all plaintext connections
```

**Effect**: Even if iptables were bypassed, the sidecar rejects any non-mTLS connection.

#### Layer 3: Kubernetes NetworkPolicy

```yaml
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: query-gateway-ingress
spec:
  podSelector:
    matchLabels:
      app: query-gateway
  policyTypes:
  - Ingress
  ingress:
  - from:
    - namespaceSelector:
        matchLabels:
          istio-injection: enabled
    ports:
    - protocol: TCP
      port: 8080
```

**Effect**: Kubernetes firewall blocks traffic that shouldn't reach the pod at all.

#### Layer 4: Pod Security Standards

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: query-gateway
spec:
  securityContext:
    runAsNonRoot: true
    seccompProfile:
      type: RuntimeDefault
  containers:
  - name: gateway
    securityContext:
      allowPrivilegeEscalation: false
      capabilities:
        drop:
        - ALL
        - NET_ADMIN  # Prevent iptables modification
        - NET_RAW
```

**Effect**: Prevents containers from tampering with iptables rules or bypassing network controls.

### Defense in Depth Summary

| Layer | What it does | Attack it prevents |
|---|---|---|
| **iptables (istio-init)** | Forces all traffic through sidecar at kernel level | Direct pod-to-pod plaintext |
| **PeerAuthentication: STRICT** | Sidecar rejects non-mTLS connections | Bypassing encryption |
| **NetworkPolicy** | K8s firewall blocks unexpected traffic | Unauthorized pod access |
| **Pod Security Standards** | Drop NET_ADMIN/NET_RAW capabilities | Container breakout, iptables tampering |

**Principle**: No single layer is sufficient. Together they make sidecar bypass effectively impossible without node-level compromise.

### For the Design Doc

> **Transport Security**: All inter-service communication uses mTLS enforced by the Istio service mesh. Istio's control plane (istiod) acts as the certificate authority, issuing short-lived SPIFFE-based identities (24h TTL) to each workload. Application code communicates over plaintext to the local sidecar proxy — mTLS is transparent to the application layer.
>
> `PeerAuthentication` policy is set to `STRICT` mode cluster-wide, rejecting any plaintext traffic between services. Sidecar bypass is prevented through defense-in-depth:
> - iptables rules (injected by `istio-init`) redirect all traffic through Envoy
> - Kubernetes NetworkPolicies restrict pod-to-pod communication
> - Pod Security Standards drop `NET_ADMIN` and `NET_RAW` capabilities
>
> External TLS termination (client → gateway) uses separately managed certificates (Let's Encrypt + cert-manager), rotated every 60 days.

---

## Security & Compliance Strategy vs. Prototype

### Where Security Lives in Deliverables

| Deliverable | Security & Compliance expectation |
|---|---|
| **Design Doc (HLA)** | **Full treatment** — TLS, mTLS, KMS, STRIDE threat model, data residency, crypto-shredding, audit trails. This is where they evaluate your *thinking*. |
| **Execution Plan** | **When** each security capability lands (e.g., M2: per-tenant KMS, M5: audit/alerts, M6: security review + pen-test readiness). |
| **Prototype** | **Minimal** — JWT auth, entitlement enforcement (RLS/CLS), and a note like "in production this would use mTLS/KMS". NOT expected: Vault, KMS, mTLS setup, STRIDE analysis. |

### Rubric Weight

**Security & Entitlements: 15%**

Evaluated primarily on:
- Design doc articulation of threat surface
- Ability to plan incremental delivery of security controls
- NOT on whether prototype has mTLS configured

### What to Include in Design Doc

1. **Transport Security**
   - TLS everywhere (client → gateway, gateway → services)
   - mTLS between internal services (Istio/Linkerd)
   - How sidecar bypass is prevented

2. **Encryption at Rest**
   - Per-tenant KMS keys via AWS KMS / GCP Cloud KMS
   - Envelope encryption for data at rest
   - Key rotation strategy

3. **Audit & Compliance**
   - Every cross-system access logged (who, what, when, tenant, connector)
   - Immutable audit store (S3 with object lock + CloudTrail)
   - Data residency tags on tenant config

4. **Tenant Isolation**
   - Namespace-per-tenant or tenant-scoped encryption
   - Network isolation (K8s NetworkPolicies)
   - Optional: single-tenant clusters for high-security customers

5. **Off-boarding & Crypto-shred**
   - Tenant deletion triggers KMS key deletion
   - All encrypted data becomes unrecoverable
   - Running jobs cancelled, cache purged

6. **STRIDE Threat Model**
   - Table mapping each category to components + mitigations

7. **Pen-test Readiness**
   - Hardened containers (distroless base images)
   - Dependency scanning (Snyk/Dependabot)
   - RBAC for cluster access
   - Network policies limiting pod-to-pod traffic

### What to Include in Execution Plan

- **M2**: Per-tenant KMS encryption, observability v1
- **M5**: Audit/alerts, multi-tenant hardening
- **M6**: Security review, pen-test readiness, chaos drills

### What to Include in Prototype

- **Implemented**: JWT auth, entitlement enforcement (RLS/CLS)
- **README note**: "Production deployment adds mTLS via Istio, per-tenant KMS encryption, and audit logging — see Design Doc section 4.3"
- **Do NOT implement**: Vault, KMS integration, mTLS setup, full STRIDE analysis
- **Do document**: Secrets taxonomy, envelope encryption pattern, DEK lifecycle, per-tenant CMK isolation, audit log key governance (see section below)

---

---

## Secrets & Key Management Architecture

### Principle 1 — Secrets Taxonomy: Vault vs. KMS

The system maintains two distinct categories of secrets, each managed by a purpose-built tool:

| Secret Type | Tool | Rationale |
|---|---|---|
| Tenant connector credentials (API keys, OAuth tokens) | HashiCorp Vault | Full lifecycle management: leasing, TTL, dynamic generation, rotation, revocation, audit trail |
| Encryption master keys (CMKs) | AWS KMS | Keys never leave HSM; IAM-integrated access control; cryptographic operations only |

**Principle**: Vault manages the lifecycle of secrets that applications consume. KMS manages the lifecycle of cryptographic keys that protect data at rest. These are not interchangeable roles.

---

### Principle 2 — Envelope Encryption for Data at Rest

All data written to the Query Cache (Redis) and Materialization Store is protected via envelope encryption. Raw data is never encrypted directly with a KMS master key.

**Pattern**:
1. Before writing, call `KMS.GenerateDataKey(CMK_ARN)` — returns `(plaintext_DEK, encrypted_DEK)`.
2. Encrypt data locally using `plaintext_DEK`; immediately discard `plaintext_DEK` from memory.
3. Persist `[encrypted_data || encrypted_DEK]` together — they are inseparable.
4. On read, call `KMS.Decrypt(encrypted_DEK)` to recover `plaintext_DEK`, then decrypt data locally.

**Why not encrypt directly with KMS?** KMS has throughput limits (~30K req/s per region) and adds network latency per operation. Envelope encryption reduces KMS calls to one per data key, not one per row.

**In-process DEK cache**: To reduce KMS round-trips on read, cache the `plaintext_DEK` in an in-process LRU cache (e.g., via AWS Encryption SDK Caching CMM) with the following bounds:

| Parameter | Purpose | Recommended Value |
|---|---|---|
| `max_age` | Maximum DEK cache lifetime | 5–15 min |
| `max_messages_encrypted` | Max reuse count per DEK | 100–1,000 |
| `max_bytes_encrypted` | Max data volume encrypted per DEK | 100 MB |

**Cache key scoping**: The DEK cache key must be `(tenant_id, data_classification)` — never shared across tenants.

**Why KMS SDK is embedded, not a sidecar**: Encryption sits in the hot path of every cache read and write. A sidecar boundary would require serializing data over localhost for every operation — unacceptable latency at 1k QPS. The plaintext DEK must live in the same process memory performing the encryption. Additionally, encryption decisions are tightly coupled to application logic: which fields to encrypt, per-tenant policy scoping, when to generate a new DEK vs. reuse a cached one. This is business logic, not a uniform infrastructure concern.

---

### Principle 3 — Vault Agent Sidecar: Data Plane Secret Access

The data plane must not call the control plane at query time to retrieve connector credentials. This would introduce runtime coupling, increase query latency, and create a single point of failure.

**Design**:
- A Vault Agent sidecar runs alongside each Query Executor instance.
- At startup, the agent authenticates to Vault once (via Kubernetes service account JWT) and fetches all tenant connector credentials scoped to that instance.
- Credentials are mounted as files under `/vault/secrets/` inside the container.
- The Query Executor reads credentials from the local filesystem — zero network calls to Vault at query time.
- The sidecar watches lease TTLs and silently refreshes credentials in the background before expiry.

**Access boundary**: Vault ACL policies enforce that the data plane holds a read-only policy scoped to `secret/tenants/*`. The control plane holds a write policy. Neither plane calls the other at runtime.

**Why sidecar over embedded Vault client**: The sidecar pattern is justified when three criteria hold simultaneously:

1. **Cross-cutting concern** — the capability is needed by every service identically (secret fetching, mTLS termination, log shipping).
2. **Stable, simple interface** — the contract between sidecar and app rarely changes (file on disk, proxied TCP).
3. **Off-the-shelf binary** — you deploy maintained software (Vault Agent, Envoy), not custom code.

Vault Agent meets all three. An embedded Vault client library (Go SDK in the Query Executor) achieves the same functional outcome but shares a failure domain with the application: a memory leak or deadlock in token renewal logic takes down query serving. The sidecar is a separate process — it can crash and restart independently.

**Counter-example — why connectors are NOT sidecars**: Connectors have domain-specific logic (pagination, schema mapping, error handling) that varies per SaaS API and participates in query planning, backpressure, and cancellation. They fail all three sidecar criteria. The general rule: sidecar for infrastructure concerns identical across services; in-process for anything that participates in business logic.

**No application-level caching of secrets**: The Query Executor reads credentials from the file mount on every call. This adds ~1-2ms (OS page cache serves the read from memory after first access — no disk I/O). Caching secrets in application memory would create a second staleness window: Vault → file (sidecar TTL) → in-memory map (app TTL). When a token rotates, two TTLs must expire instead of one. The added complexity yields no meaningful performance gain.

---

### Principle 4 — Control Plane / Data Plane Secret Boundary

The interface between control plane and data plane for secrets is Vault itself, not a REST API.

| Phase | Actor | Action |
|---|---|---|
| Provisioning (one-time) | Control Plane (Admin Service) | OAuth flow with SaaS app → writes token to `secret/tenants/{tid}/{connector}` in Vault |
| Query execution (every request) | Data Plane (Query Executor) | Reads from Vault Agent sidecar's local mount — no control plane involvement |
| Token rotation | Control Plane | Detects expiry → fetches new token → writes new version to Vault; sidecar propagates transparently |

**Anti-pattern to avoid**: Data plane calling `GET /internal/credentials/{tenant_id}` on a control plane API endpoint. This couples availability, adds latency, and violates plane isolation.

---

### Principle 5 — DEK Lifecycle Ownership

AWS KMS generates Data Encryption Keys on demand but does not store, track, or manage them. The application owns DEK persistence and lifecycle.

**Rules**:
- The `encrypted_DEK` must be stored durably, co-located with (or indexed to) the data it encrypted.
- Loss of `encrypted_DEK` = permanent, unrecoverable data loss. There is no fallback.
- Cache data (Redis) and materialized store data have different durability requirements:

| Data Type | DEK Scope | `encrypted_DEK` Storage | Data Loss If DEK Lost |
|---|---|---|---|
| Query Cache (Redis) | Per-tenant, short TTL | Alongside Redis entry | Acceptable — re-fetch from source |
| Materialization Store | Per-tenant, batch | PostgreSQL metadata table | Recoverable — re-materialize from source |
| Audit Logs (S3) | Per-batch or per-day | DynamoDB (separate from S3 object) | Catastrophic — permanent loss |

**Audit log `encrypted_DEK` storage**: Store in a dedicated DynamoDB table keyed by `batch_id`, not in S3 object metadata. S3 metadata can be overwritten; DynamoDB provides point-in-time recovery and conditional writes.

---

### Principle 6 — Per-Tenant CMK Isolation

Each tenant is assigned a dedicated KMS Customer Managed Key (CMK). Tenant data keys (DEKs) are wrapped only by that tenant's CMK.

**Benefits**:
- Compromise of one tenant's CMK does not affect other tenants.
- Crypto-shredding on tenant off-boarding: disable and schedule deletion of the tenant's CMK; all DEKs wrapped by it become permanently undecryptable without touching the data itself.
- Regulatory isolation: per-tenant CMK ARNs appear in CloudTrail, providing per-tenant key usage audit trails.

**Cost**: ~$1/CMK/month in AWS KMS. At 100s of tenants, this is negligible.

**Naming convention**: `alias/queryfed/{tenant_id}/{env}` (e.g., `alias/queryfed/acme-corp/prod`).

---

### Principle 7 — Audit Log Key Governance

Audit logs require stricter key governance than operational data because they cannot be re-derived from source systems.

**Rules**:
1. Use a **separate CMK** for audit logs, distinct from the CMK used for cache or materialized store. A misconfiguration in one domain does not affect audit integrity.
2. **Never delete audit CMKs**. Use `DisableKey` if a key must be retired; schedule deletion only after verified migration to a new key and re-encryption of all audit data under the new key.
3. Set the KMS **deletion waiting period to 30 days** (maximum) for audit CMKs. The default 7-day window is insufficient for incident investigation workflows.
4. Store `encrypted_DEK` entries in DynamoDB with **point-in-time recovery enabled** and a **retention policy of ≥ 7 years** to meet common compliance requirements (SOC 2, HIPAA).
5. Enable **KMS key rotation** on audit CMKs (annual). KMS retains all prior key versions, so previously encrypted DEKs remain decryptable.

---

## Why the Evaluators Care

As an **Eng Lead**, you're expected to:
- Understand the full threat surface
- Plan incremental, pragmatic delivery of controls
- Balance security rigor with velocity
- Communicate strategy clearly to leadership and IC teams

The design doc proves you can think at this level. The prototype proves you can ship working code. They complement each other — the prototype doesn't need to be production-grade on security, but the design doc does.
