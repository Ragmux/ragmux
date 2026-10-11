## Summary

<!-- What changed and why, in 1-3 sentences. -->

## Related Issue

<!-- "Closes #<number>" or "None". -->

## How it was tested

<!--
The commands you ran and what they covered. The Go tests are SKIPPED when
TEST_DATABASE_URL is unset, so make sure they actually ran.
-->

```
make test
```

## Impact on existing deployments

<!-- Delete the lines that do not apply. -->

- [ ] No impact: behavior is unchanged for anyone upgrading.
- [ ] Adds a migration in `internal/store/migrations/` (new numbered file, schema version bumped in the tests).
- [ ] Adds or changes an environment variable or default.
- [ ] Changes the `/v1` or `/admin/api` surface (endpoint, field, header, error code).
- [ ] **Breaking change**: described above, with the upgrade path.

## Checklist

- [ ] The pull request title follows Conventional Commits.
- [ ] `gofmt -l .` prints nothing, and `go vet ./...` and `golangci-lint run ./...` are clean.
- [ ] Tests were added or updated (or are not needed), and `make test` passes against a real PostgreSQL.
- [ ] Documentation under `docs/` was updated (or is not needed).
- [ ] User-facing changes are added to `CHANGELOG.md` under `[Unreleased]`.
- [ ] If this is a breaking change, the title contains `!` and the change is described above.
- [ ] No secrets, real provider keys, `.env` files or database dumps are included.
- [ ] I agree to license this contribution under AGPL-3.0-or-later.
