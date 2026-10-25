# Contributing to Ragmux

Thank you for your interest in contributing. This guide explains how to propose changes.
The full developer guide is
[Contributing and Development](https://github.com/ragmux/ragmux/wiki/Contributing-and-Development)
on the wiki: the local loop, how tests find their database, the schema contract and how
decisions are recorded.

## Before You Start

- Small fixes (typos, broken links, an obviously wrong default) can be sent directly as a pull request.
- For a new feature or a change in behavior, open an issue first so the approach can be discussed before you write code. The idea may already be on the [roadmap](ROADMAP.md) or deliberately out of scope.
- For a bug, open an issue with the version (`ragmux -version` or `/healthz`), how you run it, the provider involved and the steps to reproduce.
- Opening a pull request does not guarantee that it will be accepted.
- Do not report security vulnerabilities in issues or pull requests; follow the [security policy](SECURITY.md).

## Development Setup

Requirements:

- Go 1.27.3 or newer
- Docker, for the test database

Clone the repository, start the test database and run the gateway:

```bash
git clone https://github.com/ragmux/ragmux.git
cd ragmux
make dev-db     # pgvector Postgres on localhost:5433
make run        # builds and runs on :8765
```

## Running Tests

These are the same commands that CI runs. Run them before opening a pull request.

```bash
gofmt -l .                  # must print nothing
go vet ./...
golangci-lint run ./...     # golangci-lint v2.14.0, config in .golangci.yml
go run golang.org/x/vuln/cmd/govulncheck@v1.9.0 ./...
```

```bash
make test
```

`make test` runs `go test -race -count=1 ./...` against the database from `make dev-db`.
Tests are skipped, not failed, when `TEST_DATABASE_URL` is unset, so check that yours
actually ran. A change that touches the search code also needs `make test-paradedb`, which
expects ParadeDB on port 5434; start it with `make dev-db-paradedb` first.

A change to `web/index.html` also needs the syntax check from the CI `test` job, which
extracts the inline script and runs `node --check` on it:

```bash
python3 -c "import re;s=open('web/index.html').read();open('/tmp/dash.js','w').write(re.search(r'<script>(.*)</script>',s,re.S).group(1))"
node --check /tmp/dash.js
```

## Commit Messages

This project follows [Conventional Commits 1.0.0](https://www.conventionalcommits.org/en/v1.0.0/):

```text
<type>(<scope>)!: <description>

<body>
```

- **Types:** `feat`, `fix`, `docs`, `style`, `refactor`, `perf`, `test`, `build`, `ci`, `chore`, `revert`.
- **Scope** is optional, lowercase kebab-case, and names a module or area: `feat(auth): ...`.
- **Description** is in English, imperative mood, starts with a lowercase letter, has no trailing period and is at most 72 characters.
- **Body** is optional and explains why the change was made. Wrap lines at 72 characters.
- Mark a breaking change only with `!` after the type or scope, and describe it in the body.
- Do not add footers or trailers (`BREAKING CHANGE:`, `Refs:`, `Closes #12`, `Co-Authored-By:`, `Signed-off-by:` or any tool attribution). Link issues in the pull request description instead.
- Keep one logical change per commit.

Example:

```text
fix(api): return 404 for unknown project slug
```

## Pull Requests

1. Create a branch from `main` named `<type>/<short-kebab-description>`, for example `feat/token-refresh` or `fix/123-null-slug-404`.
2. Keep a pull request to one topic. Write its title in Conventional Commits format. Pull requests are squash merged, so the title becomes the commit message on `main`.
3. New behavior comes with a test. A change users can see (an environment variable, an endpoint, a header, a response field) also goes in the matching page under `docs/`.
4. If the change affects users, add an entry under `## [Unreleased]` in [CHANGELOG.md](CHANGELOG.md).
5. Fill in the pull request template and link the related issue with `Closes #<number>`. Say so in the description if the change touches the API surface, the schema or a default.
6. Make sure CI passes. A maintainer reviews the pull request and may ask for changes before merging.

Ragmux is licensed under AGPL-3.0-or-later. By contributing you agree that your contribution is licensed under the same terms. There is no CLA.

## Code of Conduct

This project is governed by the [Code of Conduct](CODE_OF_CONDUCT.md). By participating, you agree to uphold it.
