# Deployment Strategy

## Approach: Managed Services First

We use managed cloud services for CI/CD and infrastructure rather than self-hosting tools like ArgoCD or Helm operators. This is an enterprise-grade security product — operational reliability of the deployment pipeline itself is critical, and managed services provide that with minimal headcount.

**Rationale:**
- Core differentiation is the query layer and connector SDK, not deployment tooling
- Self-hosting CD (ArgoCD/Flux) would require at least 2 dedicated engineers and still may not match the reliability of a managed platform
- Managed services provide built-in canary, blue-green, and automated rollback — battle-tested at scale
- Reduces blast radius of infra incidents; vendor SLAs backstop our own

## Architecture

```
GitHub Actions (CI)                Managed Kubernetes
───────────────────────────────────────────────────────────────────────
Push → Test → Build image →  Push to Container Registry
                                      │
                              Managed CD Service
                                      │
                              Managed K8s handles:
                                - Pod autoscaling (HPA)
                                - Node provisioning
                                - Resource bin-packing
```

## Pipeline

- **CI**: GitHub Actions — test, build, push Docker image to container registry
- **CD**: Managed CD (Google Cloud Deploy or AWS CodePipeline) with pipeline targets: dev → staging → prod
- **Rollout**: Canary (10% → 50% → 100%) with SLO-gated promotion
- **Rollback**: Automated on error-rate breach via cloud monitoring alerts
- **Blue-green**: Supported natively by managed CD for zero-downtime releases
- **Autoscaling**: HPA config per service (CPU target ~70%, min 2 / max 20 replicas)
- **Infra-as-Code**: Terraform modules for VPC, managed k8s cluster, managed database, secrets, IAM

## What Terraform Manages vs What's Managed-for-You

| Terraform (us)                  | Managed by Cloud Provider       |
|---------------------------------|---------------------------------|
| VPC, subnets, firewall rules    | Node scaling (Autopilot/Fargate)|
| K8s cluster creation            | Pod scheduling                  |
| Database, Secrets Manager       | Canary traffic splitting        |
| IAM roles, service accounts     | Rollback orchestration          |
| Deploy pipeline definition      | Certificate management          |

**GCP stack**: GKE Autopilot, Cloud Deploy, Artifact Registry, Cloud SQL, Secret Manager
**AWS stack**: EKS Fargate, CodePipeline + CodeDeploy, ECR, RDS, Secrets Manager

## Future Decision Point

Evaluate whether to bring CD in-house (ArgoCD/Flux) based on:
- Customer demand for single-tenant on-prem deployments
- Need for custom rollout strategies beyond what managed CD supports
- Team growth that justifies the operational overhead

Until then, managed services are the pragmatic choice.
