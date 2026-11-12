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
| `acm.sync/dns-enabled` | User | No | Set to `"true"` to sync DNS records for the certificate's SANs. Requires `--dns-provider` |
| `acm.sync/dns-target` | User | No | Record target. Overrides `--dns-default-target` and the CloudFront domain |
| `acm.sync/dns-record-type` | User | No | `CNAME` or `ALIAS`. Defaults to `--dns-default-record-type` |
| `acm.sync/dns-ttl` | User | No | Record TTL in seconds. Defaults to `--dns-default-ttl` |
| `acm.sync/dns-adopt` | User | No | Set to `"true"` to take ownership of an existing unowned record that already points at the target |
| `acm.sync/dns-records` | Controller | — | Comma-separated FQDNs whose records this resource owns |

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

## DNS Records

The controller can also create the DNS records for a certificate's SANs, so a hostname starts resolving to its CloudFront distribution (or any other target) without a separate DNS step. It is **off by default** and supports one provider per deployment: `route53` or `dnsimple`.

It needs two opt-ins: the controller runs with `--dns-provider` (and `--dns-zones`), and each Secret or Certificate sets `acm.sync/dns-enabled: "true"` (Secret wins over Certificate). DNS sync runs after ACM and CloudFront, and is non-fatal: failures emit a `DNSSyncFailed` event and increment `dns_sync_errors_total`. Rate limits and 5xx responses are retried after 5 minutes.

### Target

Per record, the first of these that is set:

1. `acm.sync/dns-target`
2. `--dns-default-target`
3. The domain name of the CloudFront distribution in `acm.sync/cloudfront-distribution-arn` (requires `--enable-cloudfront-sync` and `cloudfront:GetDistribution`)

With none, the record is skipped with a `DNSRecordSkipped` event.

### Ownership

For every record the controller writes a TXT record at `_acm-sync.<fqdn>` (`_acm-sync.<zone>` for the apex, `_acm-sync._wildcard.<parent>` for wildcards) with the value `heritage=cert-manager-acm-sync,owner=<owner-id>,resource=secret/<namespace>/<name>`. The controller only updates or deletes records whose TXT names its own owner ID and resource. Set a unique `--dns-owner-id` per controller deployment.

| Existing state at the name | Result |
|---|---|
| Nothing | Owner TXT and record created |
| Record and TXT are ours | Updated if type, target or TTL differ, otherwise unchanged |
| Unowned CNAME/ALIAS already pointing at the target | Left alone (`unmanaged_ok`), or adopted when `acm.sync/dns-adopt: "true"` (owner TXT written, record untouched) |
| Unowned record pointing elsewhere, other record types, or a TXT owned by someone else | `DNSRecordConflict` warning, never modified |

A CNAME and an ALIAS pointing at the same target are treated as equal. The names the controller owns are recorded in `acm.sync/dns-records` on the Secret and the Certificate.

### Apex, wildcards and deletion

- **Apex**: a SAN equal to a managed zone always becomes an `ALIAS`. DNSimple uses its native ALIAS record. Route53 writes an alias `A` + `AAAA` pair, which supports CloudFront targets and targets inside the same zone only.
- **Wildcards**: skipped by default. `--dns-wildcard-policy=create` manages them.
- **Delete policy**: `--dns-delete-policy=retain` (default) never removes records. With `delete`, names dropped from the certificate are pruned, and deleting the Certificate (or the Secret when no Certificate owns it) removes its records and owner TXTs before it goes away. The controller adds the `acm.sync/dns-cleanup` finalizer for this, only on opted-in resources and only while the policy is `delete`; it removes it again if the policy goes back to `retain`. Records are only deleted where the owner TXT matches.

### Provider setup

**DNSimple**: create an API token and store it in a Secret you manage, then reference it from the chart (`dns.dnsimple.existingSecret`). The controller reads it from the `DNSIMPLE_TOKEN` environment variable; the chart never creates the Secret.

```bash
kubectl -n cert-manager-acm-sync create secret generic dnsimple-token --from-literal=token=<api-token>
```

**Route53**: uses the controller's IRSA role. Zones need their hosted zone ID (`--dns-zones=example.com=Z123EXAMPLE`). Add, scoped to the hosted zone, with the record-type condition as its own statement:

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": ["route53:ChangeResourceRecordSets", "route53:ListResourceRecordSets"],
      "Resource": "arn:aws:route53:::hostedzone/Z123EXAMPLE"
    },
    {
      "Effect": "Allow",
      "Action": "route53:ChangeResourceRecordSets",
      "Resource": "arn:aws:route53:::hostedzone/Z123EXAMPLE",
      "Condition": {
        "ForAllValues:StringEquals": {
          "route53:ChangeResourceRecordSetsRecordTypes": ["A", "AAAA", "CNAME", "TXT"]
        }
      }
    }
  ]
}
```

To derive the target from a CloudFront distribution, also allow `cloudfront:GetDistribution` on that distribution.

### Values

```yaml
dns:
  provider: dnsimple
  zones:
    - name: example.com
  deletePolicy: delete
  dnsimple:
    accountId: "1234"
    existingSecret:
      name: dnsimple-token
```

The chart fails to render when the provider is unknown, no zones are set, a route53 zone has no `id`, or dnsimple is missing `accountId` or `existingSecret.name`.

### Example

```yaml
apiVersion: cert-manager.io/v1
kind: Certificate
metadata:
  name: my-cloudfront-cert
  annotations:
    acm.sync/enabled: "true"
    acm.sync/region: "us-east-1"
    acm.sync/cloudfront-distribution-arn: "arn:aws:cloudfront::123456789012:distribution/EDFDVBD6EXAMPLE"
    acm.sync/dns-enabled: "true"
spec:
  secretName: my-cloudfront-cert
  dnsNames:
    - www.example.com
  issuerRef:
    name: letsencrypt-prod
    kind: ClusterIssuer
```

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
| `dns.provider` | `""` | DNS provider: `route53` or `dnsimple`. Empty disables DNS sync |
| `dns.zones` | `[]` | Managed zones as `{name, id}`. `id` is required for route53 |
| `dns.ownerId` | `default` | Owner ID written into ownership TXT records; unique per deployment |
| `dns.defaultTarget` | `""` | Record target when `acm.sync/dns-target` is not set |
| `dns.defaultTTL` | `300` | Default record TTL in seconds |
| `dns.defaultRecordType` | `CNAME` | `CNAME` or `ALIAS` |
| `dns.wildcardPolicy` | `skip` | `skip` or `create` |
| `dns.deletePolicy` | `retain` | `retain` or `delete` (prune and cleanup on deletion, adds a finalizer) |
| `dns.dnsimple.accountId` | `""` | DNSimple account ID. Required for dnsimple |
| `dns.dnsimple.baseUrl` | `""` | DNSimple API base URL. Empty uses `https://api.dnsimple.com` |
| `dns.dnsimple.existingSecret.name` | `""` | Existing Secret holding the DNSimple token. Required for dnsimple |
| `dns.dnsimple.existingSecret.key` | `token` | Key in that Secret |
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
| `dns_sync_total` | Counter | `provider`, `action` | DNS outcomes per name (`created` / `updated` / `unchanged` / `adopted` / `unmanaged_ok` / `conflict` / `skipped_wildcard` / `no_zone` / `deleted` / `error`) |
| `dns_sync_errors_total` | Counter | `provider`, `error_type` | Failed DNS operations (`sync` / `retryable` / `target` / `extract_sans`) |
