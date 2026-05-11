## [0.6.2](https://github.com/camilorivera/cert-manager-acm-sync/compare/v0.6.1...v0.6.2) (2026-05-11)


### Bug Fixes

* correct trivy-action version tag to v0.36.0 ([e95eb90](https://github.com/camilorivera/cert-manager-acm-sync/commit/e95eb903ca93049f3b96d21a8685ffd4b35ec07c))
* remove invalid issues.exclude-rules from golangci-lint v2 config ([eed8841](https://github.com/camilorivera/cert-manager-acm-sync/commit/eed8841e370ecc11391d7c3f9a30a8971f1ad026))
* upgrade Go to 1.25 and oauth2 to v0.27.0 to resolve Trivy CVEs ([4530b1b](https://github.com/camilorivera/cert-manager-acm-sync/commit/4530b1b26e9cdf3c4ef83a46d81b1429dcd3e627))
* upgrade golangci-lint to v2.12.2 and fix Go 1.25 compatibility ([5d1f97a](https://github.com/camilorivera/cert-manager-acm-sync/commit/5d1f97a183e1ba256611802af3704a52c2a1f11b))
* upgrade golangci-lint-action to v7 for golangci-lint v2 support ([9d58c1d](https://github.com/camilorivera/cert-manager-acm-sync/commit/9d58c1d824eacc64bb0f672e03fde5bf7879e4c6))

## [0.6.1](https://github.com/camilorivera/cert-manager-acm-sync/compare/v0.6.0...v0.6.1) (2026-05-08)


### Bug Fixes

* add cert-manager.io/certificates RBAC to Helm chart ClusterRole/Role ([f70133c](https://github.com/camilorivera/cert-manager-acm-sync/commit/f70133cf62ee21746e7435cec1abcf4e57496189))

# [0.6.0](https://github.com/camilorivera/cert-manager-acm-sync/compare/v0.5.1...v0.6.0) (2026-05-08)


### Bug Fixes

* backfill acm.sync/arn onto Certificate on skip path for pre-existing certs ([cc25e21](https://github.com/camilorivera/cert-manager-acm-sync/commit/cc25e2157ebf485625df051767e5f28f027d3b50))
* gofmt alignment in regionCapturingPool literal ([505a5f1](https://github.com/camilorivera/cert-manager-acm-sync/commit/505a5f11d3fdedaaa70ccd0fea6e433d76395646))


### Features

* support acm.sync annotations directly on cert-manager Certificate resource ([1f78aa7](https://github.com/camilorivera/cert-manager-acm-sync/commit/1f78aa7c9b15b4ac7241bd9967fd9ac6cd67c529))
* support metadata.annotations in certificates Helm values ([d2f2fbe](https://github.com/camilorivera/cert-manager-acm-sync/commit/d2f2fbee4bbd5ab45025a6405e21f3208deae1f6))

## [0.5.1](https://github.com/camilorivera/cert-manager-acm-sync/compare/v0.5.0...v0.5.1) (2026-05-08)

# [0.5.0](https://github.com/camilorivera/cert-manager-acm-sync/compare/v0.4.1...v0.5.0) (2026-05-08)


### Features

* recover ACM ARN from cert-manager Certificate when Secret is recreated ([b9dd114](https://github.com/camilorivera/cert-manager-acm-sync/commit/b9dd114ced14149a67dc768cc0e5657325f1cf37))

## [0.4.1](https://github.com/camilorivera/cert-manager-acm-sync/compare/v0.4.0...v0.4.1) (2026-05-08)


### Bug Fixes

* remove conventionalcommits preset to fix missing module error ([198e356](https://github.com/camilorivera/cert-manager-acm-sync/commit/198e3563699d7c77fbd67f96613610a1b5020298))
* restrict cache to release namespace when rbac.clusterScoped=false ([bedd5c0](https://github.com/camilorivera/cert-manager-acm-sync/commit/bedd5c04bcfa3b3be5d3d645eca9d90b5718cddf))

# [0.4.0](https://github.com/camilorivera/cert-manager-acm-sync/compare/v0.3.1...v0.4.0) (2026-05-08)


### Features

* **helm:** add certificates list to values for cert-manager Certificate resources ([d31e2e7](https://github.com/camilorivera/cert-manager-acm-sync/commit/d31e2e75a7674af42a6b61d37ef44bcede847a7a))

## [0.3.1](https://github.com/camilorivera/cert-manager-acm-sync/compare/v0.3.0...v0.3.1) (2026-05-08)


### Bug Fixes

* push Helm chart to separate GHCR path to avoid overwriting container image ([2bce805](https://github.com/camilorivera/cert-manager-acm-sync/commit/2bce80566757139a71233c61e582f91c00394522))

# [0.3.0](https://github.com/camilorivera/cert-manager-acm-sync/compare/v0.2.0...v0.3.0) (2026-05-08)


### Features

* move container image from Docker Hub to GHCR ([19195e3](https://github.com/camilorivera/cert-manager-acm-sync/commit/19195e3dcf388fb3be98f6e05fae002a201ab711))

# [0.2.0](https://github.com/camilorivera/cert-manager-acm-sync/compare/v0.1.1...v0.2.0) (2026-05-08)


### Features

* publish Helm chart to GHCR as OCI artifact instead of gh-pages ([b6bd121](https://github.com/camilorivera/cert-manager-acm-sync/commit/b6bd1219e8d2cb1ef51b7caac336c82ec443ae10))


### Performance Improvements

* cross-compile arm64 natively instead of running under QEMU ([35a9009](https://github.com/camilorivera/cert-manager-acm-sync/commit/35a9009708fd49319e6f8166241ca24899c9a33f))

## [0.1.1](https://github.com/camilorivera/cert-manager-acm-sync/compare/v0.1.0...v0.1.1) (2026-05-08)


### Performance Improvements

* remove -a flag from Dockerfile build to allow incremental Go cache hits ([9d14828](https://github.com/camilorivera/cert-manager-acm-sync/commit/9d14828230889daca10ceaaab742af12b678f35f))
