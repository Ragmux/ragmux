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
- the published Docker images (all-in-one and `-app`), `Dockerfile`, `Dockerfile.aio`,
  `docker/aio/entrypoint.sh`, `docker-compose.yml` and `docker-compose.split.yml`

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
did not come from our release pipeline. Both images are signed the same way: the
all-in-one image under `:<version>` / `:latest` and the gateway-only image under
`:<version>-app` / `:latest-app`. The signature is attached to the manifest digest,
so it also covers the `:<major>.<minor>` and `:latest` (`-app`) tags that point at the
same release. Pinning deployments to the digest reported by `cosign verify` (or by
`docker buildx imagetools inspect`) protects against a tag being moved later. The same
check works for `docker.io/ragmux/ragmux` when the Docker Hub mirror is published.
Releases before 0.3.1 carry no signature.

## The all-in-one image

The default image (`ghcr.io/ragmux/ragmux:<version>`) bundles PostgreSQL 17 with
pgvector next to the gateway on a Debian base (`pgvector/pgvector:pg17`, pinned by
digest in `Dockerfile.aio`). Two consequences for your threat model:

- **Trust authentication on the unix socket.** The embedded server has no TCP listener
  (`listen_addresses = ''`) and `pg_hba.conf` is `local all all trust` plus
  `host all all all reject`. Anything that can open `/var/run/postgresql` inside the
  container is the database superuser. That is acceptable because the container runs
  nothing but the gateway (as the unprivileged `ragmux` user), Postgres (as `postgres`)
  and the entrypoint, and because the socket is only exposed through the optional
  `ragmux-pgsocket` volume that the `backup` profile mounts. Do not mount that volume
  into other services, and treat `docker exec` into the container as superuser access.
  Deployments that need password or TLS authentication between gateway and database
  belong on the split layout (`docker-compose.split.yml`) or an external database with
  the `-app` image.
- **A full Debian userland.** Unlike the distroless `-app` image, the all-in-one image
  contains a shell, `apt`-installed libraries and the PostgreSQL toolchain, so it has
  a larger CVE surface. Scan it with the same tooling you use for the
  `pgvector/pgvector` / `postgres` images (Trivy, Grype, Docker Scout); the SBOM and
  provenance attestations are attached to both images by the release workflow. Rebuilds
  of the base image are picked up when we bump the digest in `Dockerfile.aio`.

The `-app` image stays distroless (`gcr.io/distroless/static-debian12:nonroot`, a
single static binary, no shell) for environments where that matters.

## Hardening pointers

The defaults are conservative, but a production deployment should read these sections
of [Configuration](docs/configuration.md):

- [Private upstreams](docs/configuration.md#private-upstreams): the SSRF guard and when
  to allow a hostname through it
- [Behind a reverse proxy](docs/configuration.md#behind-a-reverse-proxy): TLS,
  `TRUST_PROXY_HEADERS`, `TRUSTED_PROXY_CIDRS`, `SECURE_COOKIES`
- [Database privileges](docs/configuration.md#database-privileges): the minimum the
  gateway's role needs
- [Deployment layouts](docs/configuration.md#deployment-layouts): what the all-in-one
  container does with `/data`, and when to prefer the split layout
- [Environment variables](docs/configuration.md#environment-variables) and
  [Docker Compose variables](docs/configuration.md#docker-compose-variables): keeping
  `SECRET_KEY` and `DATABASE_URL` in secrets files (`*_FILE`) instead of `.env`

- [Formats and parsing](docs/rag.md#formats-and-parsing): uploaded documents are parsed
  under size, page and time caps and PDFs in a disposable child process; keep
  `MAX_UPLOAD_MB` modest and give upload rights only to members you trust
- [Login protection](docs/users-and-limits.md#login-protection) and the
  [audit log](docs/users-and-limits.md#audit-log): the limiter's counters
  (`GET /admin/api/security/logins`) and the NDJSON export for keeping the trail beyond
  `AUDIT_RETENTION_DAYS`

Also keep `SECRET_KEY` with your backups and rotate it with `ragmux rotate-key` when it
may have leaked ([Backup and restore](docs/backup-restore.md#rotating-secret_key)).
