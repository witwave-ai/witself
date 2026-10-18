<!-- witself-security-scan v1 status=findings go=1 npm_prod=1 npm_toolchain=1 images=1 incomplete=0 -->
# Witself security scan
Time: 2026-10-12T13:17:00Z
Commit: unknown
Tools: govulncheck v1.7.0, npm 10.0.0-stub, Grype synthetic

## Go (govulncheck, called vulnerabilities)
| Module | ID | Package |
| . | GO-0000-0001 | example.invalid/module |
Findings: 1. Informational: 0. Scanned: 2 of 2.

## npm production dependencies (moderate or above)
| Directory | Package | ID |
| infra/cloudflare/a | production-package | GHSA-aaaa-bbbb-cccc |
Findings: 1. Informational: 0. Scanned: 1 of 1.

## npm toolchain (high or above)
| Directory | Package | ID |
| infra/cloudflare/a | toolchain-package | GHSA-dddd-eeee-ffff |
Findings: 1. Informational: 0. Scanned: 1 of 1.

## Images (Critical or High with a fix)
<!-- targets:begin -->
| Source | Image | Tag | Digest |
| cell:civo-sandbox-use1-serving | ghcr.io/witwave-ai/images/witself-server | 0.0.333 | sha256:aaaaaaaaaaaa |
<!-- targets:end -->
| Source | Image | ID | Package |
| cell:civo-sandbox-use1-backup | ghcr.io/witwave-ai/images/witself-postgres-backup:0.0.292 | CVE-0000-0001 | image-package |
Findings: 1. Informational: 0. Scanned: 3 of 3.
