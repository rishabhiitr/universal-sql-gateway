# Universal SQL Query Layer — High-Level Design

> **Scenario**: GitHub Pull Requests ↔ Jira Issues (cross-app JOIN)
> **Author**: Rishabh Mor
> **Date**: 2026-02-28

---

## Table of Contents

1. [Overview & Goals](#1-overview--goals)
2. [Architecture Overview](#2-architecture-overview)
3. [Control Plane ↔ Data Plane Interaction](#3-control-plane--data-plane-interaction)
4. [Deep-Dive Docs](#4-deep-dive-docs)

---

## 1. Overview & Goals

### Problem

Enterprise teams use dozens of SaaS applications — GitHub, Jira, Salesforce, Zendesk, Notion, and hundreds more. Data that answers real business questions (e.g., "which open PRs are blocking high-priority issues?") lives across multiple systems with incompatible APIs, auth models, rate limits, and data shapes. Today, getting that answer requires a custom integration or a manual export.

This system provides a **universal SQL query layer**: users write a single SQL SELECT statement, and the system handles federation, auth, rate-limit compliance, freshness control, and entitlement enforcement transparently.

### Scale Targets

| Dimension | Target |
|---|---|
| Customers | 100s of enterprise tenants |
| Connector types | 1,000s of SaaS app types |
| Users | Up to 10M |
| Peak throughput | ~1,000 QPS |
| Data throughput | ~100 MB/s |
| Latency P50 | < 500 ms (single-source, predicate pushdown) |
| Latency P95 | < 1.5 s (single-source, predicate pushdown) |
| Query Gateway availability | 99.9% monthly |

### Prototype Scenario

```sql
SELECT gh.title, gh.state, j.issue_key, j.status, j.assignee
FROM github.pull_requests gh
JOIN jira.issues j ON gh.jira_issue_id = j.issue_key
WHERE gh.state = 'open'
LIMIT 10
```

Joins GitHub PRs with Jira issues on a shared key, filtered to open PRs, with entitlement enforcement, rate-limit handling, and freshness control.

### Out of Scope

- DML (INSERT, UPDATE, DELETE) — read-only query layer
- Full SQL dialect (CTEs, window functions, subqueries) — SELECT/WHERE/JOIN/ORDER BY/LIMIT subset
- Connector write-back — connectors are read-only data sources

---

## 2. Architecture Overview

The system is split into two planes with a clear boundary between them.

```
┌─────────────────────────────────────────────────────────────────────┐
│                         CONTROL PLANE                               │
│                                                                     │
│  ┌─────────────┐  ┌──────────────┐  ┌───────────┐  ┌───────────┐  │
│  │  Tenant &   │  │   Schema     │  │  Policy   │  │  Secrets  │  │
│  │  Connector  │  │   Catalog    │  │   Store   │  │  & KMS    │  │
│  │  Registry   │  │  (Postgres)  │  │  (OPA /   │  │  (Vault + │  │
│  │             │  │              │  │   YAML)   │  │   Cloud   │  │
│  └─────────────┘  └──────────────┘  └───────────┘  │   KMS)    │  │
│                                                     └───────────┘  │
│  ┌─────────────────────────────────────────────────────────────┐   │
│  │  Rate-Limit Policy Store  │  Audit Log  │  Admin Console    │   │
│  └─────────────────────────────────────────────────────────────┘   │
└─────────────────────────────────────────────────────────────────────┘
                              │ config & policy reads (30-60s TTL)
                              ▼
┌─────────────────────────────────────────────────────────────────────┐
│                          DATA PLANE                                 │
│                                                                     │
│  ┌──────────────────────────────────────────────────────────────┐  │
│  │                      Query Gateway                           │  │
│  │         AuthN (OIDC/JWT)  │  AuthZ (policy check)           │  │
│  │         Request shaping   │  Timeout enforcement            │  │
│  └──────────────────────────┬───────────────────────────────────┘  │
│                             │                                       │
│  ┌──────────────────────────▼───────────────────────────────────┐  │
│  │                      Query Planner                           │  │
│  │   SQL parse → capability discovery → predicate pushdown      │  │
│  │   join planning → cost/freshness hints → execution plan      │  │
│  └──────────────────────────┬───────────────────────────────────┘  │
│                             │                                       │
│         ┌───────────────────┼───────────────────┐                  │
│         │                   │                   │                  │
│  ┌──────▼──────┐   ┌────────▼────────┐  ┌──────▼──────┐          │
│  │  Entitlement│   │  Rate-Limit     │  │  Freshness  │          │
│  │  Service    │   │  Service        │  │  & Cache    │          │
│  │  (RLS/CLS)  │   │  (token bucket) │  │  Layer      │          │
│  └─────────────┘   └─────────────────┘  └─────────────┘          │
│                                                                     │
│  ┌──────────────────────────────────────────────────────────────┐  │
│  │                   Connector Workers                          │  │
│  │                                                              │  │
│  │  ┌────────────────┐      ┌────────────────┐      ┌───────┐  │  │
│  │  │ GitHub         │      │ Jira           │      │  ...  │  │  │
│  │  │ Connector      │      │ Connector      │      │       │  │  │
│  │  └────────┬───────┘      └───────┬────────┘      └───┬───┘  │  │
│  └───────────┼──────────────────────┼───────────────────┼──────┘  │
│              │                      │                   │          │
└──────────────┼──────────────────────┼───────────────────┼──────────┘
               │                      │                   │
               ▼                      ▼                   ▼
          GitHub API             Jira API            Other SaaS APIs
```

### Component Responsibilities

| Component | Responsibility |
|---|---|
| **Query Gateway** | AuthN (OIDC/JWT), request validation, timeout enforcement, response shaping. Single entry point per tenant. |
| **Query Planner** | Parse SQL, discover connector capabilities, classify predicates (pushdown vs local), build join plan, emit execution plan. |
| **Entitlement Service** | Merge source permissions + tenant policy → compute RLS row filters + CLS column masks per user. Applied post-fetch. |
| **Rate-Limit Service** | Token bucket per `(tenant, connector, user)`. Enforces API rate-limit budgets. Returns `RetryAfter` on exhaustion. Routes overflow to async queue. |
| **Freshness & Cache Layer** | Fetch-level cache keyed on pushed predicates. `max_staleness` per query. ETag/conditional fetch. Two-tier: source cache (Redis) + materialization cache (S3/DuckDB). |
| **Connector Workers** | Stateless goroutines executing fetches concurrently. Implement the `Connector` interface: `Fetch`, `Schema`, `Tables`. Handle auth token refresh, pagination, and connector-specific errors. |
| **Schema Catalog** | Postgres-backed store of connector schemas, tenant configs, policy definitions. Migrations via Atlas/Flyway. |
| **Secrets & KMS** | HashiCorp Vault for secret storage + cloud KMS (AWS KMS / GCP Cloud KMS) for per-tenant envelope encryption. Rotation and break-glass access. |
| **Audit Log** | Immutable append-only log (S3 + object lock) of every cross-system access: who, what table, which tenant, connector, result row count, timestamp. |
| **Admin Console** | Config-driven connector onboarding (YAML or UI). Connector versioning. Rate-limit policy management. Tenant off-boarding triggers. |

---

## 3. Control Plane ↔ Data Plane Interaction

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
| Credentials (OAuth tokens, API keys) | In-memory in connector | Until expiry or 401 | Vault Agent sidecar; refresh on 401 |

> Full staleness inventory and what happens when cached data is stale: `docs/control-plane-design-notes.md`

---

## 4. Deep-Dive Docs

| Topic | Doc |
|---|---|
| Request lifecycle: component map, gateway, planner, sync/async paths, end-to-end trace | `docs/data-plane/request-lifecycle.md` |
| Connector: deployment model, SDK interface, runtime isolation, bulkhead patterns | `docs/data-plane/connector.md` |
| Executor: entitlement enforcement (OPA/OPAL), RLS/CLS, join execution, spill, sync/async | `docs/data-plane/executor.md` |
| Rate-limit service: token bucket design, fairness, async overflow path | `docs/data-plane/rate-limit-service.md` |
| Freshness & cache layer: TTL, ETag/conditional fetch, two-tier cache, materialization | `docs/data-plane/freshness-and-caching.md` |
| Join execution: hash join, sort-merge, spill strategy | `docs/data-plane/join-engine.md` |
| Memory management: accounting, goroutine pools, resource provisioning | `docs/data-plane/memory-and-resource-mgmt.md` |
| Prototype vs production gap | `docs/data-plane/production-gaps.md` |
| Control plane: schema catalog, connector registry, Postgres schema, Vault | `docs/control-plane-design-notes.md` |
| Security & compliance: mTLS, KMS, STRIDE threat model, audit, crypto-shred | `docs/security-design-notes.md` |
| Capacity & performance: sizing math, autoscaling, SLO budget | `docs/capacity-and-performance.md` |
| Six-month execution plan | `docs/EXECUTION_PLAN.md` |
| Prototype implementation plan | `docs/IMPLEMENTATION_PLAN.md` |
