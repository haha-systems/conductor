# Contributing to Conductor

## Running the tests

```bash
go test ./...
go test -race ./...
```

## Branch naming

Follow the existing convention:

| Prefix   | Use for                          |
|----------|----------------------------------|
| `feat/`  | New features                     |
| `fix/`   | Bug fixes                        |
| `ci/`    | CI / workflow changes            |
| `chore/` | Maintenance, deps, docs, cleanup |

## Pull request process

1. Fork the repo and create a branch from `main` using the naming convention above.
2. Make your changes and ensure `go test ./...` passes.
3. Open a PR against `main` — CI runs tests and a build check automatically.
4. A maintainer will review and merge once CI is green.

## Adding a new provider

1. Create a file in `internal/provider/` (e.g. `myprovider.go`).
2. Implement the `AgentProvider` interface defined in `internal/provider/provider.go`.
3. Register the provider in `buildProviders` in `cmd/conductor/main.go`.
4. Add an entry to the Providers table in `README.md`.

## Adding a new work source

1. Create a file in `internal/worksource/` (e.g. `myplatform.go`).
2. Implement the `WorkSource` interface defined in `internal/worksource/worksource.go`.
3. Register the source in `buildWorkSource` in `cmd/conductor/main.go`.
4. Add configuration documentation to `README.md` under **Work sources**.
