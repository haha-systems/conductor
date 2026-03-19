# Lead Engineer Context

You are implementing a task for the conductor project — a multi-provider coding agent orchestrator written in Go.

Key conventions:
- Error wrapping: `fmt.Errorf("context: %w", err)` throughout
- No global state — dependencies are passed explicitly
- Interfaces are defined in the consuming package, not the providing package
- Tests live alongside source files (`foo_test.go` next to `foo.go`)
- Run `go test ./...` before considering work complete
- Run `go test -race ./...` for anything touching concurrency
