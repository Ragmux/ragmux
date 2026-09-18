# Security policy

Ragmux sits between your users and your provider API keys, so we treat reports
seriously and fix them quickly. Thank you for helping keep it safe.

## Supported versions

| Version | Supported |
|---|---|
| 0.3.x | yes |
| < 0.3 | no, upgrade to the latest 0.3.x |

Fixes ship as patch releases of the current minor version; older lines do not receive
backports.

## Reporting a vulnerability

Please do **not** open a public issue for security problems. Use one of:

- GitHub private vulnerability reporting:
  <https://github.com/ragmux/ragmux/security/advisories/new>
- Email: <security@ragmux.com>

Include the version (`ragmux -version` or `/healthz`), the deployment shape (Docker
Compose, bare binary, reverse proxy), steps to reproduce and the impact you see. Proof
of concept code is welcome; please keep testing to systems you own.

## What to expect

- Acknowledgement within **3 business days**.
- A fix or a documented mitigation within **30 days** for high and critical issues;
  lower severities are scheduled into the next release.
- We keep you updated while the report is open and tell you when the fix ships.

## Scope

In scope:

- the gateway binary (`/v1` client API, `/admin` management API, background jobs)
- the embedded dashboard (`web/index.html`)
- the scripts in `scripts/` (backup, restore, dev helpers)
- the published Docker image, `Dockerfile` and `docker-compose.yml`

Out of scope:

- vulnerabilities in third-party providers (OpenAI, Anthropic, Gemini, Ollama, ...) or
  in the models themselves
- deployments that ignore the documented hardening: the gateway exposed without TLS or a
  reverse proxy, `ALLOW_PRIVATE_UPSTREAMS=true` on an untrusted network, a database
  role with more privileges than documented, secrets committed to `.env` in a repository
- prompt injection inherent to LLMs, beyond the mitigations documented in
  [Retrieval (RAG)](docs/rag.md) (retrieved passages are labelled as untrusted data)
- denial of service that requires a valid project API key and stays within the
  configured rate limits and budgets

## Disclosure

We follow coordinated disclosure: the fix is released first, then the advisory is
published with credit to the reporter in the release notes (unless you prefer to stay
anonymous). Please give us the response window above before publishing details.

## Verifying the container image

Images published from **v0.3.1 onward** are signed with [cosign](https://docs.sigstore.dev/)
(keyless, Sigstore/Fulcio through the release workflow's GitHub OIDC identity). Verify
a tag before pulling it into production:

```sh
cosign verify ghcr.io/ragmux/ragmux:<tag> \
  --certificate-identity-regexp '^https://github.com/ragmux/ragmux/.github/workflows/release.yml@refs/tags/v' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

A successful run prints the signature payload and the certificate's identity, which must
be `release.yml` in this repository running for a `v*` tag; anything else means the image
did not come from our release pipeline. The signature is attached to the manifest digest,
so it also covers the `:<major>.<minor>` and `:latest` tags that point at the same
release. Pinning deployments to the digest reported by `cosign verify` (or by
`docker buildx imagetools inspect`) protects against a tag being moved later. The same
check works for `docker.io/ragmux/ragmux` when the Docker Hub mirror is published.
Releases before 0.3.1 carry no signature.

## Hardening pointers

The defaults are conservative, but a production deployment should read these sections
of [Configuration](docs/configuration.md):

- [Private upstreams](docs/configuration.md#private-upstreams): the SSRF guard and when
  to allow a hostname through it
- [Behind a reverse proxy](docs/configuration.md#behind-a-reverse-proxy): TLS,
  `TRUST_PROXY_HEADERS`, `TRUSTED_PROXY_CIDRS`, `SECURE_COOKIES`
- [Database privileges](docs/configuration.md#database-privileges): the minimum the
  gateway's role needs
- [Environment variables](docs/configuration.md#environment-variables) and
  [Docker Compose variables](docs/configuration.md#docker-compose-variables): keeping
  `SECRET_KEY` and `DATABASE_URL` in secrets files (`*_FILE`) instead of `.env`

- [Login protection](docs/users-and-limits.md#login-protection) and the
  [audit log](docs/users-and-limits.md#audit-log): the limiter's counters
  (`GET /admin/api/security/logins`) and the NDJSON export for keeping the trail beyond
  `AUDIT_RETENTION_DAYS`

Also keep `SECRET_KEY` with your backups and rotate it with `ragmux rotate-key` when it
may have leaked ([Backup and restore](docs/backup-restore.md#rotating-secret_key)).
