# cert-manager-acm-sync

A Kubernetes controller that automatically syncs TLS certificates issued by [cert-manager](https://cert-manager.io) into [AWS Certificate Manager (ACM)](https://aws.amazon.com/certificate-manager/), with support for automatic renewal without changing the ACM certificate ARN.

## How it works

1. cert-manager issues a TLS certificate and stores it as a `kubernetes.io/tls` Secret.
2. You annotate the Secret (or the cert-manager `Certificate` resource's `secretTemplate`) with `acm.sync/enabled: "true"`.
3. The controller imports the certificate into ACM and writes the ARN back as `acm.sync/arn` on both the Secret and the owning `Certificate` resource.
4. When cert-manager renews the certificate, the controller detects the fingerprint change and re-imports to the **same ARN** — ALBs, CloudFront, and API Gateway require no reconfiguration.
5. If the Secret is deleted and recreated by cert-manager, the controller recovers the ARN from the owning `Certificate` annotation and reimports to the **same ARN** instead of creating a new certificate.
6. If the ACM certificate is deleted externally, the controller detects the stale ARN and creates a new certificate.

## Installation

### Helm (recommended)

```bash
helm repo add cert-manager-acm-sync https://camilorivera.github.io/cert-manager-acm-sync
helm install cert-manager-acm-sync cert-manager-acm-sync/cert-manager-acm-sync \
  --namespace cert-manager-acm-sync \
  --create-namespace \
  --set serviceAccount.annotations."eks\.amazonaws\.com/role-arn"=arn:aws:iam::ACCOUNT_ID:role/cert-manager-acm-sync \
  --set controller.defaultRegion=us-east-1
```

### Raw YAML

```bash
kubectl apply -f config/manager/service_account.yaml
kubectl apply -f config/rbac/
kubectl apply -f config/manager/manager.yaml
```

## Annotation Reference

| Annotation | Set by | Description |
|---|---|---|
| `acm.sync/enabled: "true"` | User | **Required** — opt-in trigger |
| `acm.sync/region: "us-east-1"` | User | Optional — target AWS region. **Required for CloudFront** (must be `us-east-1`) |
| `acm.sync/arn` | Controller | ACM certificate ARN written after first import |
| `acm.sync/fingerprint` | Controller | SHA-256 of leaf cert DER bytes; used for change detection |
| `acm.sync/last-sync` | Controller | RFC3339 timestamp of last successful sync |

## Quick Start

### With cert-manager Certificate resource

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
  dnsNames:
    - my-service.example.com
  issuerRef:
    name: letsencrypt-prod
    kind: ClusterIssuer
```

### CloudFront (must be in us-east-1)

```yaml
acm.sync/enabled: "true"
acm.sync/region: "us-east-1"
```

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

## Values

| Key | Default | Description |
|---|---|---|
| `replicaCount` | `2` | Number of controller replicas (leader election active) |
| `image.repository` | `camilorivera/cert-manager-acm-sync` | Docker image repository |
| `image.tag` | `""` (uses appVersion) | Image tag |
| `controller.defaultRegion` | `us-east-1` | Default AWS region |
| `controller.leaderElect` | `true` | Enable leader election |
| `serviceAccount.annotations` | `{}` | Annotations for IRSA |
| `rbac.create` | `true` | Create RBAC resources |
| `rbac.clusterScoped` | `true` | ClusterRole (all namespaces) vs Role (release namespace only) |

## Observability

Prometheus metrics exposed on `:8080/metrics`:

- `acm_sync_total{region, action}` — sync operations (import / reimport / skipped)
- `acm_sync_errors_total{region, action}` — sync errors
- `acm_sync_last_sync_timestamp{region, secret}` — unix timestamp of last successful sync
