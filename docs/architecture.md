# Development Architecture Contract

Shuhari separates the Agent Skills evaluation mechanism from repository-specific gate policy and agent-specific runtime behavior. Maintainers should use these boundaries to decide whether new behavior belongs in Shuhari, a consuming repository, or an agent adapter.

## Responsibility boundaries

Shuhari owns:

- strict eval, instruction, and trigger schemas;
- clean per-trial workspaces and fixture staging;
- agent invocation through a narrow harness and an agent-neutral execution-security contract;
- blind assertion grading and A/B comparison;
- durable output, transcript, timing, grading, benchmark, and failure artifacts;
- per-case trial aggregation that tolerates minority variance, and cache entries only for passing results.

The consuming repository owns:

- which changed files select a skill or instructions target;
- pre-commit hook definitions and `SKIP` behavior;
- declared policy values such as `--trials`, `--jobs`, `--timeout`, `--network`, and `--strict-all-trials`;
- static checks for repository-specific naming, migration, or ownership rules.

The selected agent CLI owns authentication, model configuration, native sandbox enforcement, and its event format. Each Shuhari adapter maps the neutral execution-security contract to those native controls, refuses policies it cannot honor, and converts native events into the internal result contract.

Two integration boundaries remain open. The staged-target wrapper that maps pre-commit changes to Shuhari targets belongs to each consuming repository. Document slop review cannot import Go internals, and its runner and subprocess boundary is not yet specified. Authoring calibration loops, viewers, and servers remain separate tools outside Shuhari.

## Packages

```text
.
├── cmd/shuhari/
└── internal/
    ├── cache/
    ├── cli/
    ├── eval/
    │   └── prompts/
    ├── harness/
    ├── skill/
    └── trigger/
```

- `cmd/shuhari` is the executable entry point and receives the release version through linker flags.
- `internal/cli` defines only the public command tree, flags, output, and exit-code mapping.
- `internal/skill` reads and validates shared `SKILL.md` metadata.
- `internal/eval` loads skill or instructions cases, stages fixtures, runs candidate/baseline pairs, grades them, aggregates trials, and writes the Agent Skills workspace.
- `internal/trigger` loads near-miss and positive cases, measures target reads, applies trigger policy, and writes trigger evidence.
- `internal/harness` defines the agent-neutral request, result, and security-resolution boundary and contains the Codex adapter. It gives the Codex client an isolated `CODEX_HOME`, gives model-generated commands a separate minimal environment, invokes `codex exec --ephemeral --json`, and parses structured events.
- `internal/cache` stores only successful results. Cache keys include the runner binary so evaluator changes invalidate prior results.

There is no public Go SDK or dynamic plugin registry. A new agent is added as a concrete harness implementation only after it passes the adapter conformance suite. The CLI schemas, workspace layout, and repository policy remain independent of that adapter.

## Evaluation flow

1. The CLI resolves and strictly validates the target and its cases.
2. The engine resolves run and judge security, then completes the adapter preflight before creating a workspace.
3. The engine creates a new `iteration-N` directory and schedules a bounded number of candidate/baseline runs.
4. Every run receives a fresh temporary Git repository and isolated agent home. Fixtures are copied into the repository; evaluator-only fields such as `expected_output` are not included in the run prompt. Produced files and the final response are copied into the durable workspace.
5. A structured grader receives blinded A/B artifacts. Its response is checked for complete case, trial, assertion, and quoted-evidence coverage before grades are accepted.
6. A separate blind comparator receives the original task and both artifacts. Its unblinded mapping and decision are stored independently from assertion grades.
7. Candidate assertion results are aggregated per case. The default is majority; `--strict-all-trials` requires all trials. Required actions always require every trial. The comparison also requires candidate wins to be at least baseline wins.
8. `benchmark.json` records candidate-minus-baseline differences and assertion audit categories. A passing result enters the cache; a failure retains evidence but never enters the cache.

Passing evidence is `strong` when its normalized quote occurs in the artifact. Otherwise, a quote of at least eight tokens is a grounded `paraphrase` when its token LCS recall is at least 0.75 within an artifact window no longer than twice the quote; `grading.json` records the score and best-matching normalized artifact span. Evidence below either bar is a `hallucination` and fails closed.

Trigger checks measure whether the agent reads the target `SKILL.md`; they do not grade output quality. Each trial uses the same isolation and bounded scheduling as an evaluation run. Read evidence accumulates only from successful pre-response commands that reference the target. The body is split into nonblank, byte-weighted chunks of at most 128 bytes; a read requires at least 90% cumulative coverage. This tolerates a truncated tail while rejecting metadata-only commands and materially incomplete reads. A case passes when at least `floor(trials/2)+1` outcomes match `should_trigger`; `--strict-all-trials` raises that requirement to every trial.

## Execution security contract

Shuhari exposes neutral security levels. Native agent mode names are adapter details, not accepted `--sandbox` or `SHUHARI_SANDBOX` values.

### Neutral sandbox levels

| Level | Filesystem guarantee | Network guarantee |
| --- | --- | --- |
| `isolated` | Commands can write only inside the evaluation workspace; other exposed paths are read-only. | Denied unless `--network=true`. |
| `read-only` | Commands cannot write to the workspace or host. | Denied unless `--network=true`. |
| `unsandboxed` | No filesystem guarantee. | No egress guarantee; requires `--network=true`. |

### Credential boundary

`credential_boundary: enforced` means model-generated commands cannot read source or temporary agent credentials. `none` makes no such claim. Protected levels require `enforced`; `unsandboxed` records `none` and requires `SHUHARI_I_UNDERSTAND_NO_CREDENTIAL_BOUNDARY=1`.

All modes strip known GitHub credential variables from child commands. This is only a mitigation in `unsandboxed`; same-UID commands can still read credential files.

### Adapter resolution and refusal

An explicit `--sandbox` wins over `SHUHARI_SANDBOX`. `ResolveSecurity` returns one digest-bearing adapter mapping; the engine validates and reuses it for runs, artifacts, and cache keys. `Probe` checks the native sandbox before workspace creation. Unsupported mappings or hosts return `ErrUnsupportedSecurityPolicy`; Shuhari never degrades.

Judges use `read-only` without network. When the evaluated run is `unsandboxed`, the judge also uses acknowledged `unsandboxed` with network because a protected judge cannot run on that host.

### Security provenance

Schema-v2 manifests and verdicts record the neutral level, network access, credential boundary, adapter, native mode, and policy digest. Evaluation manifests also record `judge_security`. The v2 cache key includes both resolutions, adapter and runner identity, suite inputs, configuration, and judge prompts.

## Codex adapter

| Shuhari level | Codex implementation | Recorded credential boundary |
| --- | --- | --- |
| `isolated` | Codex `workspace-write` plus the Shuhari permission profile | `enforced` |
| `read-only` | Codex `read-only` plus the Shuhari permission profile | `enforced` |
| `unsandboxed` | Codex `danger-full-access` | `none` |

Protected runs give the Codex client a private temporary home while child commands receive a minimal environment that denies source and temporary Codex homes. Claude Code and Gemini CLI remain unsupported until their adapters pass conformance.

## Adapter contract

- `Capabilities` declares skill, instructions, and trigger-evidence support.
- `ResolveSecurity` returns an exact mapping or `ErrUnsupportedSecurityPolicy`.
- `Probe` verifies the supplied resolutions on the host and returns cache identity.
- `Run` starts a clean context and returns response, transcript, usage, timing, read evidence, and actions.

The adapter owns native flags, state isolation, retries, event parsing, and conformance proof. Repository gate policy stays outside it.

## Adapter conformance

An adapter is selectable only after tests prove:

- stable mappings, typed refusals, host preflight, and unchanged resolution reuse;
- workspace and outside-path reads/writes, symlink escapes, and source/temporary credential denial;
- denied and allowed network behavior;
- native event parsing and explicit `unsandboxed` provenance.

CI runs these probes against the real Codex child sandbox for both protected levels and both network states. Credential, filesystem, and network mutations must make the suite fail.

## Action evidence

Native successful actions retain their trace order. Shuhari also compares the workspace before and after the run so shell writes such as `cp` and redirection satisfy `file_change`. Because a final workspace diff cannot identify which command made the change, that evidence is marked order-unknown rather than appended to the trace. Required-action matching accepts it in any slot that is consistent with the known trace order. This proves that a file change occurred, but not whether it happened before or after another action; cases that require that exact relationship need native ordered evidence. Standard `gh api`, `gh search`, `gh repo`, and `gh browse` commands satisfy `github_search` without requiring a literal GitHub URL.

## Documentation growth

Keep development documents flat under `docs/` while each subject fits in one page. Expected siblings include schema references, [eval-authoring guidance](eval-authoring.md), integration boundaries, and the release process. Create a subject subdirectory only when one of those topics grows into multiple documents; do not add empty category directories in advance.
