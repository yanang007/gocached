# Unit tests stay beside their packages; only end-to-end tests move to e2e/

We considered moving all tests into a separate `tests/` directory to keep source directories clean, and rejected it. A Go test outside its package can't reach unexported identifiers. It also breaks the default per-package `go test` and coverage, and relative `testdata` paths. Only the black-box tests that build and run the `gocached` binary move to `e2e/`, with their own `testdata`. See `docs/research/refactor-components.md`.
