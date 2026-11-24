# Contributing

Thanks for helping. Issues and pull requests are welcome.

## Before you open a PR

```sh
make test   # go test -race ./...
make lint   # golangci-lint run
make chart  # helm lint and template
```

CI also runs govulncheck, kubeconform, actionlint, `terraform validate` and an end-to-end test on kind.

## Guidelines

- Keep collectors read-only. A new source should push to `/api/v1/events` or a mapper in `internal/hooks`, never require write access to the system it watches.
- Anything that could contain a secret goes through `internal/redact` before it is stored.
- Webhook mappers are pure functions: add a recorded payload to the tests.
- Ranking changes need a test that shows the new order and the reasons shown to users.
- User-facing text: plain, active voice, says what happened and what to do next.

## Commit messages

Use the imperative mood ("Add Flagsmith mapper"), and reference the issue when there is one.
