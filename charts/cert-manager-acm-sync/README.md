# cert-manager-acm-sync

A Kubernetes controller that automatically syncs TLS certificates issued by [cert-manager](https://cert-manager.io) into [AWS Certificate Manager (ACM)](https://aws.amazon.com/certificate-manager/), with support for automatic renewal without changing the ACM certificate ARN.

## How it works

1. cert-manager issues a TLS certificate and stores it as a `kubernetes.io/tls` Secret.
2. You annotate the cert-manager `Certificate` resource directly with `acm.sync/enabled: "true"` (or propagate it via `secretTemplate.annotations` — both are supported).
3. The controller imports the certificate into ACM and writes the ARN back as `acm.sync/arn` on both the Secret and the owning `Certificate` resource.
4. When cert-manager renews the certificate, the controller detects the fingerprint change and re-imports to the **same ARN** — ALBs, CloudFront, and API Gateway require no reconfiguration.
5. If the Secret is deleted and recreated by cert-manager, the controller recovers the ARN from the owning `Certificate` annotation and reimports to the **same ARN** instead of creating a new certificate.
6. On every reconcile (including skips), if the owning `Certificate` is missing the `acm.sync/arn` annotation, the controller backfills it — so existing certificates are covered automatically after a controller upgrade.
7. If the ACM certificate is deleted externally, the controller detects the stale ARN and creates a new certificate.

## Prerequisites

cert-manager must be running in the cluster before installing this chart. If you haven't installed it yet, follow the [official cert-manager installation guide](https://cert-manager.io/docs/installation/):

```bash
helm repo add jetstack https://charts.jetstack.io --force-update
helm install cert-manager jetstack/cert-manager \
  --namespace cert-manager \
  --create-namespace \
  --set installCRDs=true
```

## Installation

```bash
helm install cert-manager-acm-sync oci://ghcr.io/camilorivera/charts/cert-manager-acm-sync \
  --version <version> \
  --namespace cert-manager-acm-sync \
  --create-namespace \
  --set serviceAccount.annotations."eks\.amazonaws\.com/role-arn"=arn:aws:iam::ACCOUNT_ID:role/cert-manager-acm-sync \
  --set controller.defaultRegion=us-east-1
```

See [releases](https://github.com/camilorivera/cert-manager-acm-sync/releases) for the latest version.

## Annotation Reference

| Annotation | Set by | Required | Description |
|---|---|---|---|
| `acm.sync/enabled` | User | Yes | Set to `"true"` to opt this Secret into ACM sync |
| `acm.sync/region` | User | No | AWS region override. **Required for CloudFront** (`"us-east-1"`) |
| `acm.sync/arn` | Controller | — | ACM certificate ARN, written after first import |
| `acm.sync/fingerprint` | Controller | — | SHA-256 of the leaf cert's DER bytes, used for change detection |
| `acm.sync/last-sync` | Controller | — | RFC3339 timestamp of the last successful sync |
| `acm.sync/cloudfront-distribution-arn` | User | No | ARN of the CloudFront distribution to keep in sync. Requires `--enable-cloudfront-sync`. |

## CloudFront Integration

When `controller.enableCloudfrontSync: true` is set and `acm.sync/cloudfront-distribution-arn` is annotated on a Secret or Certificate, the controller automatically updates the CloudFront distribution after each ACM import: sets the Aliases to the certificate's DNS SANs and associates the ACM cert ARN.

The certificate must be in `us-east-1`. Add these IAM permissions to the controller's IRSA role:

```json
{
  "Effect": "Allow",
  "Action": ["cloudfront:GetDistributionConfig", "cloudfront:UpdateDistribution"],
  "Resource": "*"
}
```

CloudFront sync is **non-fatal**: failure emits a `CloudFrontSyncFailed` warning event but does not block the ACM import.

## Quick Start

### Annotate the Certificate resource (recommended)

Add `acm.sync/enabled: "true"` directly to the `Certificate` resource. This is the preferred approach — the annotation lives on the resource that represents your intent, not on the Secret:

```yaml
apiVersion: cert-manager.io/v1
kind: Certificate
metadata:
  name: my-service-tls
  namespace: default
  annotations:
    acm.sync/enabled: "true"
    # acm.sync/region: "us-east-1"  # required for CloudFront
spec:
  secretName: my-service-tls
  dnsNames:
    - my-service.example.com
  issuerRef:
    name: letsencrypt-prod
    kind: ClusterIssuer
```

### Via Helm values

```yaml
certificates:
  - name: my-service-tls
    spec:
      secretName: my-service-tls
      dnsNames:
        - my-service.example.com
      issuerRef:
        name: letsencrypt-prod
        kind: ClusterIssuer
```

> To enable ACM sync, add `acm.sync/enabled: "true"` to `metadata.annotations` in the certificate entry.

## IAM Permissions

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

> `acm:DeleteCertificate` is intentionally omitted. The controller never deletes ACM certificates.

## Values

| Key | Default | Description |
|---|---|---|
| `replicaCount` | `2` | Replicas (leader election keeps only one active) |
| `image.repository` | `ghcr.io/camilorivera/cert-manager-acm-sync` | Container image |
| `image.tag` | `""` | Defaults to the chart's `appVersion` |
| `controller.defaultRegion` | `us-east-1` | Default AWS region for ACM imports |
| `controller.leaderElect` | `true` | Enable leader election |
| `controller.enableCloudfrontSync` | `false` | Enable CloudFront distribution alias sync after ACM imports |
| `serviceAccount.annotations` | `{}` | Use to set the IRSA role ARN |
| `rbac.create` | `true` | Create RBAC resources |
| `rbac.clusterScoped` | `true` | `true` = ClusterRole (all namespaces), `false` = Role (release namespace only) |
| `podDisruptionBudget.enabled` | `true` | Create a PodDisruptionBudget |
| `certificates` | `[]` | cert-manager `Certificate` resources to create alongside the controller |

## Observability

Prometheus metrics exposed on `:8080/metrics`:

| Metric | Type | Labels | Description |
|---|---|---|---|
| `acm_sync_total` | Counter | `region`, `action` | Sync operations (`import` / `reimport` / `skipped`) |
| `acm_sync_errors_total` | Counter | `region`, `action` | Failed sync operations |
| `acm_sync_last_sync_timestamp` | Gauge | `region`, `secret` | Unix timestamp of last successful sync |
| `cloudfront_sync_total` | Counter | `action` | CloudFront sync operations (`synced`) |
| `cloudfront_sync_errors_total` | Counter | `error_type` | Failed CloudFront sync operations (`get_config` / `update` / `extract_id` / `extract_sans`) |
