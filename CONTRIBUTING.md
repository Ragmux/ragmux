# Contributing to Ragmux

Bug reports, documentation fixes and pull requests are all welcome. This page is the
short version; the full developer guide is
[Contributing and Development](https://github.com/Ragmux/ragmux/wiki/Contributing-and-Development)
on the wiki — what to install, the local loop, how tests find their database, the schema
contract and how decisions are recorded.

Please **do not** open a public issue for a security problem. Follow
[SECURITY.md](SECURITY.md) instead.

## Before you start

- **Bugs:** open an issue with the version (`ragmux -version` or `/healthz`), how you run it,
  the provider involved and the steps to reproduce.
- **Features:** open an issue describing the problem before writing code. The idea may
  already be on the [roadmap](ROADMAP.md) or deliberately out of scope.
- **Small fixes** (a typo, a broken link, an obviously wrong default) need no issue. Send the
  pull request.

## The local loop

You need **Go 1.27.1 or newer** and Docker for the test database.

```bash
make dev-db     # pgvector Postgres on localhost:5433
make test       # unit and end-to-end tests, with the race detector
make run        # builds and runs on :8765
```

Tests are **skipped, not failed,** when `TEST_DATABASE_URL` is unset, so check that yours
actually ran. Anything that touches the search code also needs `make test-paradedb`, which
expects ParadeDB on port 5434; start it with `make dev-db-paradedb` first.

## Before you push

CI runs these on every pull request; run them locally first.

```bash
gofmt -l .                  # must print nothing
go vet ./...
golangci-lint run ./...     # v2, config in .golangci.yml
govulncheck ./...
make test
```

## Commits and pull requests

- Write commit subjects in the imperative mood, describing the change rather than the file:
  `Add summary comparison and breakdown options`. The repository does not use Conventional
  Commits.
- Keep a pull request to one topic, and rebase on `main` instead of merging it back in.
- Fill in the pull request template: what changed, why, how you tested it, and whether it
  affects an existing deployment.
- New behaviour comes with a test. A change users can see — an environment variable, an
  endpoint, a header, a response field — also goes in the matching page under `docs/` and in
  `CHANGELOG.md` under `Unreleased`.
- Say so in the description if the change touches the API surface, the schema or a default.

## License

Ragmux is licensed under **AGPL-3.0-or-later**. By contributing you agree that your
contribution is licensed under the same terms. There is no CLA.

Participation is governed by the
[Code of Conduct](https://github.com/Ragmux/.github/blob/main/CODE_OF_CONDUCT.md).
