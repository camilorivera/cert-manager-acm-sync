# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Development environment

There is no local Go installation required. All build, test, and lint commands run inside Docker via `docker compose`. Use explicit `docker compose run` commands — not `make` wrappers, not host `go`.

```bash
docker compose run --rm dev go mod tidy                                          # first-time / dep changes
docker compose run --rm dev go build -o bin/manager ./cmd/manager               # compile
docker compose -f docker-compose.test.yml run --rm test                         # full test suite
docker compose run --rm lint                                                     # golangci-lint
```

Run a single test:
```bash
docker compose run --rm dev go test -v -run TestReconcile ./internal/controller/
```

## Architecture

This is a Kubernetes controller (built with `controller-runtime`) that watches `kubernetes.io/tls` Secrets and imports their certificate material into AWS ACM, preserving the same ARN across cert-manager renewals.

### Reconcile flow (`internal/controller/secret_controller.go`)

The `SecretReconciler` is the core. When triggered, it:

1. Resolves the opt-in annotation (`acm.sync/enabled: "true"`) — accepted on either the Secret or its owning cert-manager Certificate (via `ownerReferences`). This allows annotating the `Certificate` resource instead of propagating it through `secretTemplate.annotations`.
2. Computes a SHA-256 fingerprint of the leaf certificate DER (`internal/fingerprint`). If the stored fingerprint matches, the reconcile is a no-op (re-queues after 6 hours).
3. Determines the target AWS region: `acm.sync/region` on the Secret takes precedence, then the same annotation on the Certificate, then the `--default-region` flag.
4. Calls `acm:ImportCertificate` (or re-imports to the existing ARN). The `acm.Client` pool (`internal/acm`) lazily creates one `acm.Client` per region, sharing IRSA credentials.
5. Patches `acm.sync/arn`, `acm.sync/fingerprint`, and `acm.sync/last-sync` back onto the Secret, and mirrors the ARN onto the owning Certificate so the ARN survives Secret deletion/recreation.
6. **Optional CloudFront sync** (`maybeSyncCloudFront`): if `--enable-cloudfront-sync` is set and `acm.sync/cloudfront-distribution-arn` is annotated (on Secret or Certificate), calls `GetDistributionConfig` + `UpdateDistribution` to set the distribution's Aliases to the cert's DNS SANs and associate the new ACM ARN. This step is non-fatal — failure logs a warning event and increments `cloudfront_sync_errors_total` but does not cause a reconcile retry.

### ARN recovery

If cert-manager deletes and recreates a Secret (e.g. on renewal), the new Secret loses the `acm.sync/arn` annotation. The controller recovers it from the Certificate's annotation and re-imports to the same ARN rather than creating a new certificate in ACM.

### Event filtering (`internal/controller/predicates.go`)

Two predicates prevent infinite reconcile loops:
- **`TLSAnnotatedPredicate`** — passes all TLS Secret creates; for updates, passes cert-data changes and annotation opt-in toggles, but suppresses updates where only `acm.sync/*` keys changed (the controller's own write-back).
- **`CertificateAnnotatedPredicate`** — secondary watch on cert-manager `Certificate` resources; maps them to reconcile requests for their `spec.secretName` Secret. Suppresses our own ARN write-backs.

### Package map

| Package | Responsibility |
|---|---|
| `cmd/manager` | Flag parsing, AWS config loading (IRSA), manager setup |
| `internal/controller` | `SecretReconciler`, predicates |
| `internal/acm` | `ACMAPI` interface, region-keyed client pool, `ImportWithRetry` with exponential backoff |
| `internal/annotations` | All `acm.sync/*` annotation keys and helpers; `StripACMAnnotations` strips only controller-written keys (ARN, fingerprint, last-sync) |
| `internal/cloudfront` | `CloudFrontAPI` interface, single global client, `SyncDistribution` (ETag-based), `DistributionIDFromARN` |
| `internal/fingerprint` | Leaf-cert SHA-256, PEM chain splitting for ACM's API, `ExtractSANs` (DNS names or CN fallback) |
| `internal/metrics` | Prometheus counters/gauges registered into controller-runtime's registry |

### Testing approach

Unit tests in `internal/acm` and `internal/fingerprint` are fully self-contained. Controller tests in `internal/controller/secret_controller_test.go` use `envtest` (a real API server) and a `MockPool` / `MockACMClient` from `internal/acm/mock_acm.go` to avoid real AWS calls.

The `test/e2e/` directory contains end-to-end tests that require a real cluster and AWS credentials — not run in CI.
