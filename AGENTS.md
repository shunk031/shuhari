# AGENTS.md

## Transport retry tests

Keep transport-failure markers in the table-driven cases in `internal/harness/codex_test.go`, beside `internal/harness/codex.go`. For every new marker, add all three controls:

- a transport failure followed by success is retried;
- failures beyond the retry limit fail; and
- a completed response is never retried.
