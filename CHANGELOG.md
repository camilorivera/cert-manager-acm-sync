# [0.2.0](https://github.com/camilorivera/cert-manager-acm-sync/compare/v0.1.1...v0.2.0) (2026-05-08)


### Features

* publish Helm chart to GHCR as OCI artifact instead of gh-pages ([b6bd121](https://github.com/camilorivera/cert-manager-acm-sync/commit/b6bd1219e8d2cb1ef51b7caac336c82ec443ae10))


### Performance Improvements

* cross-compile arm64 natively instead of running under QEMU ([35a9009](https://github.com/camilorivera/cert-manager-acm-sync/commit/35a9009708fd49319e6f8166241ca24899c9a33f))

## [0.1.1](https://github.com/camilorivera/cert-manager-acm-sync/compare/v0.1.0...v0.1.1) (2026-05-08)


### Performance Improvements

* remove -a flag from Dockerfile build to allow incremental Go cache hits ([9d14828](https://github.com/camilorivera/cert-manager-acm-sync/commit/9d14828230889daca10ceaaab742af12b678f35f))
