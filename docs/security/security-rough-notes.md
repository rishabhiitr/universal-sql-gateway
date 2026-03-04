# Security — Rough Notes

Detailed implementation notes, deep dives, and learning material. The presentable version is [security-design-notes.md](security-design-notes.md).

---

## mTLS Deep Dive: How Istio Service Mesh Works

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

### Preventing Sidecar Bypass — Full Details

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

**Principle**: No single layer is sufficient. Together they make sidecar bypass effectively impossible without node-level compromise.

---

## K8s NetworkPolicy vs Istio AuthorizationPolicy — Deep Dive

These are **independent enforcement layers**. They don't know about each other.

**K8s NetworkPolicy**: Enforced by the CNI plugin at the kernel level (iptables/eBPF). Operates at L3/L4 — matches on pod labels, translates them to IPs, blocks/allows at the packet level. Survives sidecar bypass because it runs in the kernel, not userspace.

**Istio AuthorizationPolicy**: Enforced by the Envoy sidecar in userspace. Operates at L7 — can inspect HTTP paths, headers, and most importantly, the **caller's cryptographic identity** (SPIFFE cert from mTLS). Can answer "is this actually the gateway service, or a compromised pod with matching labels?"

```yaml
# Istio AuthorizationPolicy — identity-based
apiVersion: security.istio.io/v1
kind: AuthorizationPolicy
metadata:
  name: connector-authz
  namespace: data-plane
spec:
  selector:
    matchLabels:
      role: connector-worker
  rules:
    - from:
        - source:
            principals: ["cluster.local/ns/data-plane/sa/gateway"]
```

**Why both?** NetworkPolicy stops traffic at the kernel even if Istio's sidecar is bypassed. Istio stops traffic from the right IP but the wrong service identity (e.g., a compromised pod with matching labels but a different ServiceAccount). Neither alone covers all attack vectors.

---

## Vault Agent Sidecar — Detailed Justification

### Why Sidecar Over Embedded Vault Client

The sidecar pattern is justified when three criteria hold simultaneously:

1. **Cross-cutting concern** — the capability is needed by every service identically (secret fetching, mTLS termination, log shipping).
2. **Stable, simple interface** — the contract between sidecar and app rarely changes (file on disk, proxied TCP).
3. **Off-the-shelf binary** — you deploy maintained software (Vault Agent, Envoy), not custom code.

Vault Agent meets all three. An embedded Vault client library (Go SDK in the Query Executor) achieves the same functional outcome but shares a failure domain with the application: a memory leak or deadlock in token renewal logic takes down query serving. The sidecar is a separate process — it can crash and restart independently.

### Counter-Example: Why Connectors Are NOT Sidecars

Connectors have domain-specific logic (pagination, schema mapping, error handling) that varies per SaaS API and participates in query planning, backpressure, and cancellation. They fail all three sidecar criteria. General rule: sidecar for infrastructure concerns identical across services; in-process for anything that participates in business logic.

### No Application-Level Caching of Secrets

The Query Executor reads credentials from the file mount on every call. This adds ~1-2ms (OS page cache serves the read from memory after first access — no disk I/O). Caching secrets in application memory would create a second staleness window: Vault → file (sidecar TTL) → in-memory map (app TTL). When a token rotates, two TTLs must expire instead of one. The added complexity yields no meaningful performance gain.

---

## KMS SDK: Why Embedded, Not a Sidecar

Encryption sits in the hot path of every cache read and write. A sidecar boundary would require serializing data over localhost for every operation — unacceptable latency at 1k QPS. The plaintext DEK must live in the same process memory performing the encryption.

Additionally, encryption decisions are tightly coupled to application logic: which fields to encrypt, per-tenant policy scoping, when to generate a new DEK vs. reuse a cached one. This is business logic, not a uniform infrastructure concern.

---

## DEK Lifecycle — Extended Notes

AWS KMS generates Data Encryption Keys on demand but does not store, track, or manage them. The application owns DEK persistence and lifecycle.

**Rules**:
- The `encrypted_DEK` must be stored durably, co-located with (or indexed to) the data it encrypted.
- Loss of `encrypted_DEK` = permanent, unrecoverable data loss. There is no fallback.

### Audit Log Key Governance

Audit logs require stricter key governance than operational data because they cannot be re-derived from source systems.

**Rules**:
1. Use a **separate CMK** for audit logs, distinct from the CMK used for cache or materialized store.
2. **Never delete audit CMKs**. Use `DisableKey` if a key must be retired; schedule deletion only after verified migration to a new key and re-encryption of all audit data under the new key.
3. Set the KMS **deletion waiting period to 30 days** (maximum) for audit CMKs. The default 7-day window is insufficient for incident investigation workflows.
4. Store `encrypted_DEK` entries in DynamoDB with **point-in-time recovery enabled** and a **retention policy of >= 7 years** to meet common compliance requirements (SOC 2, HIPAA).
5. Enable **KMS key rotation** on audit CMKs (annual). KMS retains all prior key versions, so previously encrypted DEKs remain decryptable.

---

## Namespace Scalability Notes

- Kubernetes officially supports up to ~10,000 namespaces per cluster (scalability targets).
- Each namespace creates objects in etcd: ResourceQuotas, NetworkPolicies, RBAC bindings, ServiceAccounts.
- At 1000s of namespaces, etcd watch pressure and API server latency increase.
- NetworkPolicy evaluation scales with total policy count across all namespaces.
- Practical for Tier 2 enterprise tenants (~100s), not for all tenants (~1000s).

---

## Temp File Protection — Volume Examples

### RAM-backed tmpfs (preferred for small materializations)
```yaml
volumes:
  - name: scratch
    emptyDir:
      medium: Memory      # RAM-backed, never hits physical disk
      sizeLimit: 512Mi    # auto-evicted if exceeded
```

### Disk-backed emptyDir (for larger joins)
```yaml
volumes:
  - name: spill
    emptyDir: {}           # disk-backed, cleaned on pod restart
```
