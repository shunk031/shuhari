# AGENTS.md

## Transport retry tests

Keep transport-failure markers in the table-driven cases in `internal/harness/codex_test.go`, beside `internal/harness/codex.go`. For every new marker, add all three controls:

- a transport failure followed by success is retried;
- failures beyond the retry limit fail; and
- a completed response is never retried.

## Prose

Markdown is checked by textlint with `@cffnpwr/textlint-rule-no-arbitrary-line-break`. Do not hard-wrap sentences; let each paragraph be one line.

This matters most for the embedded prompts under `internal/**/prompts`. No human reads them as a document, and hard-wrapping them forces every test that matches on their text to collapse whitespace first.

The gate is a pre-commit hook, matching the other repositories in this family. Run `make setup` in every new clone or worktree to install it.

`CLAUDE.md` is a symlink to this file and is excluded, as are the frozen transcript fixtures under `internal/*/testdata/`.
