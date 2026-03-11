# Deployment Strategy

## Approach: Managed Services First

We use managed cloud services for CI/CD and infrastructure rather than self-hosting tools like ArgoCD or Helm operators. This is an enterprise-grade security product, and deployment reliability matters as much as application correctness.

**Rationale:**
- Core differentiation is the query layer and connector SDK, not deployment tooling
- Self-hosting CD (ArgoCD/Flux) would require at least 2 dedicated engineers and still may not match the reliability of a managed platform
- Managed services reduce operational overhead for CI/CD, registry, secrets, and cluster management
- Reduces blast radius of infra incidents; vendor SLAs backstop our own

## Architecture

```
GitHub Actions (CI)                Managed Kubernetes
───────────────────────────────────────────────────────────────────────
Push → Test → Build image →  Push to Container Registry
                                      │
                              Managed Pipeline
                                      │
                              EKS + Kubernetes handle:
                                - Pod autoscaling (HPA)
                                - Rolling updates
                                - Pod scheduling / bin-packing
```

## Pipeline

- **CI**: GitHub Actions — test, build, push Docker image to container registry
- **CD**: Managed pipeline orchestration (Google Cloud Deploy or AWS CodePipeline) with targets: dev → staging → prod
- **Rollout**: Native Kubernetes rolling updates with readiness checks and surge/unavailable settings
- **Rollback**: Automated rollback on health-check or error-rate breach; manual promotion gate for production
- **Autoscaling**: HPA config per service (CPU target ~70%, min 2 / max 20 replicas)
- **Infra-as-Code**: Terraform modules for VPC, managed k8s cluster, node pools, managed database, secrets, IAM

## What Terraform Manages vs What's Managed-for-You

| Terraform (us)                  | Managed by Cloud Provider       |
|---------------------------------|---------------------------------|
| VPC, subnets, firewall rules    | Managed control plane           |
| K8s cluster creation            | Pod scheduling                  |
| Node pools / instance classes   | Rolling update primitives       |
| Database, Secrets Manager       | Certificate management          |
| IAM roles, service accounts     | Basic cluster autoscaling       |
| Deploy pipeline definition      | Pipeline execution / approvals  |

**GCP stack**: GKE Autopilot, Cloud Deploy, Artifact Registry, Cloud SQL, Secret Manager
**AWS stack**: EKS managed control plane + managed node groups, CodePipeline, ECR, RDS, Secrets Manager

## Future Decision Point

Evaluate whether to move beyond the initial model based on:
- Customer demand for single-tenant on-prem deployments
- Need for richer canary / blue-green semantics beyond native rolling updates
- Team growth that justifies the operational overhead
- Preference for self-managed in-cluster software (ArgoCD / Flux / Argo Rollouts) versus a managed third-party platform such as Harness

Until then, managed services plus native Kubernetes rollout primitives are the pragmatic choice.
