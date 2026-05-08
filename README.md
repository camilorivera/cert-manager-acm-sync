# cert-manager-acm-sync

A Kubernetes controller that automatically syncs TLS certificates issued by [cert-manager](https://cert-manager.io) into [AWS Certificate Manager (ACM)](https://aws.amazon.com/certificate-manager/), with support for automatic renewal without changing the certificate ARN.

## Why

AWS services like ALB, CloudFront, and API Gateway reference ACM certificates by ARN. cert-manager does not natively push certificates to ACM, and ACM-managed certificates cannot be renewed by cert-manager. This controller bridges the gap:

- cert-manager remains the **source of truth** for issuance and renewal
- ACM is the **destination** — consumed by AWS services
- Renewals are re-imported to the **same ARN**, so AWS services require no reconfiguration

## How it works

```
cert-manager ──► kubernetes.io/tls Secret ──► cert-manager-acm-sync ──► AWS ACM
                  (annotated for sync)          (controller)             (same ARN)
```

1. cert-manager issues a TLS certificate and stores it as a `kubernetes.io/tls` Secret.
2. You annotate the Secret (or the cert-manager `Certificate` resource's `secretTemplate`) with `acm.sync/enabled: "true"`.
3. The controller imports the certificate into ACM and writes the ARN back as `acm.sync/arn`.
4. When cert-manager renews the certificate, the controller detects the fingerprint change and **re-imports to the same ARN** — no downstream reconfiguration needed.
5. If the ACM certificate is deleted externally, the controller detects the stale ARN on the next reconcile and creates a new one.

## Annotation Reference

| Annotation | Set by | Required | Description |
|---|---|---|---|
| `acm.sync/enabled` | User | Yes | Set to `"true"` to opt this Secret into ACM sync |
| `acm.sync/region` | User | No | AWS region override. **Required for CloudFront** (`"us-east-1"`) |
| `acm.sync/arn` | Controller | — | ACM certificate ARN, written after first import |
| `acm.sync/fingerprint` | Controller | — | SHA-256 of the leaf cert's DER bytes, used for change detection |
| `acm.sync/last-sync` | Controller | — | RFC3339 timestamp of the last successful sync |

> **CloudFront note:** CloudFront requires certificates in `us-east-1`. Set `acm.sync/region: "us-east-1"` on Secrets used by CloudFront distributions.

## Quick Start

### 1. Annotate your cert-manager Certificate

```yaml
apiVersion: cert-manager.io/v1
kind: Certificate
metadata:
  name: my-service-tls
  namespace: default
spec:
  secretName: my-service-tls
  secretTemplate:
    annotations:
      acm.sync/enabled: "true"
      # acm.sync/region: "us-east-1"  # uncomment for CloudFront
  dnsNames:
    - my-service.example.com
  issuerRef:
    name: letsencrypt-prod
    kind: ClusterIssuer
```

Or annotate an existing TLS Secret directly:

```yaml
kubectl annotate secret my-tls acm.sync/enabled=true
```

### 2. Install the controller

**Helm (recommended):**

```bash
helm install cert-manager-acm-sync oci://ghcr.io/camilorivera/cert-manager-acm-sync \
  --version 0.1.1 \
  --namespace cert-manager-acm-sync \
  --create-namespace \
  --set serviceAccount.annotations."eks\.amazonaws\.com/role-arn"=arn:aws:iam::ACCOUNT_ID:role/cert-manager-acm-sync \
  --set controller.defaultRegion=us-east-1
```

**Raw YAML:**

```bash
kubectl apply -f config/manager/service_account.yaml
kubectl apply -f config/rbac/
kubectl apply -f config/manager/manager.yaml
```

### 3. Verify

```bash
# Check the controller is running
kubectl -n cert-manager-acm-sync get pods

# Watch the annotation appear on the Secret
kubectl get secret my-service-tls -o jsonpath='{.metadata.annotations}' | jq
# Expected output includes:
# "acm.sync/arn": "arn:aws:acm:us-east-1:123456789012:certificate/..."
# "acm.sync/fingerprint": "..."
# "acm.sync/last-sync": "..."
```

## AWS Setup (EKS + IRSA)

### IAM policy

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": ["acm:ImportCertificate"],
      "Resource": "*"
    },
    {
      "Effect": "Allow",
      "Action": ["acm:DescribeCertificate", "acm:ListCertificates"],
      "Resource": "*"
    }
  ]
}
```

> `acm:DeleteCertificate` is intentionally omitted. The controller **never deletes** ACM certificates. Deletion is always manual, preventing accidental outages on ALBs or CloudFront distributions.

### IRSA trust policy

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Principal": {
        "Federated": "arn:aws:iam::ACCOUNT_ID:oidc-provider/oidc.eks.REGION.amazonaws.com/id/OIDC_ID"
      },
      "Action": "sts:AssumeRoleWithWebIdentity",
      "Condition": {
        "StringEquals": {
          "oidc.eks.REGION.amazonaws.com/id/OIDC_ID:sub": "system:serviceaccount:cert-manager-acm-sync:cert-manager-acm-sync",
          "oidc.eks.REGION.amazonaws.com/id/OIDC_ID:aud": "sts.amazonaws.com"
        }
      }
    }
  ]
}
```

## Development

No local Go installation required. Everything runs inside Docker containers via Docker Compose. You only need **Docker Desktop** (or Docker Engine) installed.

### Prerequisites

- [Docker Desktop](https://www.docker.com/products/docker-desktop/) (or Docker Engine + Compose plugin)
- `make`

### How it works

Two Compose files define the dev environment:

| File | Purpose |
|---|---|
| `docker-compose.yml` | `dev` service for build/lint/mod commands; `lint` service for golangci-lint |
| `docker-compose.test.yml` | `test` service that runs the full test suite including controller integration tests |

Both mount the project root as `/workspace` and share persistent volume caches for the Go build cache and module cache, so repeated runs are fast.

### First-time setup

Clone the repo and resolve modules. This generates `go.sum` — **commit it** so subsequent builds are reproducible and fast.

```bash
make setup        # runs go mod tidy inside the dev container
git add go.sum
git commit -m "chore: add go.sum"
```

### Day-to-day commands

```bash
make build        # compile manager binary → bin/manager
make test-unit    # unit tests only (fast, no external deps)
make test         # full test suite including envtest integration tests
make lint         # golangci-lint
make mod-tidy     # update go.mod and go.sum
make docker-build # build the production Docker image (tag: dev)
```

All commands above run entirely inside containers — no Go, no linter, nothing installed locally.

### Running an arbitrary Go command

```bash
# Usage: make go CMD="<go subcommand and flags>"
make go CMD="vet ./..."
make go CMD="build -v ./cmd/manager"

# Or directly with Docker Compose:
docker compose run --rm dev go vet ./...
docker compose run --rm dev go build -v ./cmd/manager
```

### Volumes and caching

The Compose setup uses two named Docker volumes:

| Volume | Purpose |
|---|---|
| `go-build-cache` | Go build cache (`GOCACHE`) — speeds up incremental builds |
| `go-mod-cache` | Module download cache (`GOPATH/pkg/mod`) — avoids re-downloading |

To reset the caches (e.g. after a Go version upgrade):

```bash
docker volume rm cert-manager-acm-sync_go-build-cache cert-manager-acm-sync_go-mod-cache
```

### Integration tests (envtest)

The `make test` target first downloads the Kubernetes API server and etcd binaries needed by envtest, then runs all tests. On first run this takes ~30 seconds to download; subsequent runs use the cached binaries from `.envtest/bin/`.

```bash
make setup-envtest   # download envtest binaries only
make test            # download (if needed) + run all tests
```

### Running the controller against a real cluster

The dev container mounts the project source but not your kubeconfig or AWS credentials. The easiest way to run the controller locally against a real cluster is to pass them explicitly:

```bash
docker compose run --rm \
  -v "$HOME/.kube:/root/.kube:ro" \
  -v "$HOME/.aws:/root/.aws:ro" \
  -e AWS_PROFILE=my-profile \
  --entrypoint="" \
  dev go run ./cmd/manager \
    --default-region=us-east-1 \
    --leader-elect=false
```

Or if you prefer, export static credentials as env vars:

```bash
docker compose run --rm \
  -e AWS_ACCESS_KEY_ID \
  -e AWS_SECRET_ACCESS_KEY \
  -e AWS_SESSION_TOKEN \
  -v "$HOME/.kube:/root/.kube:ro" \
  --entrypoint="" \
  dev go run ./cmd/manager \
    --default-region=us-east-1 \
    --leader-elect=false
```

## Helm Chart Values

| Key | Default | Description |
|---|---|---|
| `replicaCount` | `2` | Replicas (leader election keeps only one active) |
| `image.repository` | `camilorivera/cert-manager-acm-sync` | Docker image |
| `image.tag` | `""` | Defaults to the chart's `appVersion` |
| `controller.defaultRegion` | `us-east-1` | Default AWS region |
| `controller.leaderElect` | `true` | Enable leader election |
| `serviceAccount.annotations` | `{}` | Use to set the IRSA role ARN |
| `rbac.create` | `true` | Create RBAC resources |
| `rbac.clusterScoped` | `true` | `true` = ClusterRole (all namespaces), `false` = Role (release namespace only) |
| `podDisruptionBudget.enabled` | `true` | Create a PodDisruptionBudget |

## Observability

### Prometheus metrics

Scraped from `:8080/metrics`:

| Metric | Type | Labels | Description |
|---|---|---|---|
| `acm_sync_total` | Counter | `region`, `action` | Sync operations (`import` / `reimport` / `skipped`) |
| `acm_sync_errors_total` | Counter | `region`, `action` | Failed sync operations |
| `acm_sync_last_sync_timestamp` | Gauge | `region`, `secret` | Unix timestamp of last successful sync |

### Kubernetes Events

The controller emits events on each managed Secret:

| Reason | Type | Description |
|---|---|---|
| `Synced` | Normal | Certificate successfully imported or re-imported |
| `SyncFailed` | Warning | AWS API error during import |
| `CertificateNotFound` | Warning | Stored ARN no longer exists in ACM; creating a new certificate |
| `MissingData` | Warning | `tls.crt` or `tls.key` absent from Secret |

### Suggested alerts

```yaml
# Prometheus alerting rules
- alert: ACMSyncErrors
  expr: rate(acm_sync_errors_total[5m]) > 0
  for: 5m
  annotations:
    summary: "ACM sync failures detected"

- alert: ACMSyncStale
  expr: time() - acm_sync_last_sync_timestamp > 86400
  annotations:
    summary: "ACM sync has not run in over 24h for {{ $labels.secret }}"
```

## Architecture

```
┌──────────────────────────────────────────────────────────────────┐
│  EKS Cluster                                                     │
│                                                                  │
│  ┌─────────────┐   TLS Secret    ┌──────────────────────────┐   │
│  │ cert-manager│────────────────►│  kubernetes.io/tls       │   │
│  │             │                 │  acm.sync/enabled: "true" │   │
│  └─────────────┘                 └────────────┬─────────────┘   │
│                                               │ Watch            │
│  ┌────────────────────────────────────────────▼─────────────┐   │
│  │  SecretReconciler                                        │   │
│  │  1. Predicate: type=tls + annotation filter              │   │
│  │  2. Fingerprint: SHA-256 of leaf cert DER                │   │
│  │  3. DescribeCertificate → detect stale ARNs              │   │
│  │  4. ImportCertificate (import / re-import / skip)        │   │
│  │  5. Patch acm.sync/* annotations back onto Secret        │   │
│  │  6. Emit Events + Prometheus metrics                      │   │
│  └──────────────────────┬───────────────────────────────────┘   │
│          ServiceAccount │ IRSA                                   │
└─────────────────────────┼──────────────────────────────────────--┘
                          │
                          ▼
            ┌─────────────────────────┐
            │  AWS ACM                │
            │  (same ARN on renewal)  │
            └─────────────────────────┘
```

## Security

- **Private keys are never logged.** The controller extracts cert fields by name and never passes `corev1.Secret` objects to log statements.
- **Patch-only access.** RBAC grants `get`, `list`, `watch`, `patch` on Secrets — no `create`, `update`, or `delete`.
- **No ACM deletion.** The controller cannot delete certificates from ACM by design.
- **IRSA scoped to this ServiceAccount.** The IAM trust policy restricts `AssumeRoleWithWebIdentity` to the exact `system:serviceaccount:cert-manager-acm-sync:cert-manager-acm-sync` principal.

## License

Apache 2.0 — see [LICENSE](LICENSE).
