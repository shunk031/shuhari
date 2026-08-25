# Development Architecture Contract

Shuhari separates the Agent Skills evaluation mechanism from repository-specific gate policy and agent-specific runtime behavior. Maintainers should use these boundaries to decide whether new behavior belongs in Shuhari, a consuming repository, or an agent adapter.

The intended readers are maintainers and gate operators. The outcome is a fresh,
auditable with/without evaluation: each side is graded in a blinded workspace,
the comparator remains blind, and security, timing, grading, and benchmark
receipts explain the certification result. The detailed grading rules live in
[`docs/grading-contract.md`](grading-contract.md).

## Responsibility boundaries

Shuhari owns:

- strict eval, instruction, and trigger schemas;
- clean per-trial workspaces and fixture staging;
- agent invocation through a narrow harness and an agent-neutral execution-security contract;
- blind assertion grading and A/B comparison;
- durable output, transcript, timing, grading, benchmark, and failure artifacts;
- per-case trial aggregation that tolerates minority variance.

The consuming repository owns:

- which changed files select a skill or instructions target;
- pre-commit hook definitions and `SKIP` behavior;
- declared policy values such as `--trials`, `--jobs`, `--timeout`, `--network`, `--allow-tool`, and `--strict-all-trials`;
- static checks for repository-specific naming, migration, or ownership rules.

The selected agent CLI owns authentication, model configuration, native sandbox enforcement, and its event format. Each Shuhari adapter maps the neutral execution-security contract to those native controls, refuses policies it cannot honor, and converts native events into the internal result contract.

Two integration boundaries remain open. The staged-target wrapper that maps pre-commit changes to Shuhari targets belongs to each consuming repository. Document slop review cannot import Go internals, and its runner and subprocess boundary is not yet specified. Authoring calibration loops, viewers, and servers remain separate tools outside Shuhari.

## Packages

```text
.
├── cmd/shuhari/
└── internal/
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
- `internal/trigger` loads near-miss and positive cases, records consultation, judges application, applies trigger policy, and writes trigger evidence.
- `internal/harness` defines the agent-neutral request, result, and security-resolution boundary and contains the Codex adapter. It gives the Codex client an isolated `CODEX_HOME`, gives model-generated commands a separate minimal environment, invokes `codex exec --ephemeral --json`, and parses structured events.

There is no public Go SDK or dynamic plugin registry. A new agent is added as a concrete harness implementation only after it passes the adapter conformance suite. The CLI schemas, workspace layout, and repository policy remain independent of that adapter.

## Evaluation flow

1. The CLI resolves and strictly validates the target and its cases.
2. The engine resolves run and judge security, then completes the adapter preflight before creating a workspace.
3. The engine creates a new `iteration-N` directory and schedules a bounded number of candidate/baseline runs.
4. Every run receives a fresh temporary Git repository and isolated agent home. Fixtures are copied into the repository; evaluator-only fields such as `expected_output` are not included in the run prompt, and the skill's `evals` directory is withheld when the skill is installed, so the definitions never reach the run through the filesystem either. Produced files and the final response are copied into the durable workspace.
5. A per-side judge agent receives only a blind label and assertions, then reads one copied artifact tree in its resolved judge workspace. Its free-form evidence is recorded as returned.
6. A separate blind comparator remains prompt-rendered and receives the original task and both artifacts. Its unblinded mapping and decision are stored independently from assertion grades.
7. Candidate assertion results are aggregated per case. The default is majority; `--strict-all-trials` requires all trials. The comparison also requires candidate wins to be at least baseline wins.
8. `benchmark.json` records candidate-minus-baseline differences and assertion audit categories.

Judge evidence is free-form supporting material and is recorded without matcher or threshold checks. Deterministic checks belong in verification scripts.

Trigger checks record consultation separately from application. Read evidence accumulates only from successful pre-response commands that reference the target. The body is split into nonblank, byte-weighted chunks of at most 128 bytes; a read requires at least 90% cumulative coverage. A trial with no read is not applied. After a read, a structured judge using the trial's resolved security classifies the transcript as applied, declined, or ambiguous; ambiguity fails closed. A case passes when at least `floor(trials/2)+1` application outcomes match `should_trigger`; `--strict-all-trials` raises that requirement to every trial.

## Execution security contract

Shuhari exposes neutral security levels. Native agent mode names are adapter details, not accepted `--sandbox` or `SHUHARI_SANDBOX` values.

### Neutral sandbox levels

| Level         | Filesystem guarantee                                                                        | Network guarantee                               |
| ------------- | ------------------------------------------------------------------------------------------- | ----------------------------------------------- |
| `isolated`    | Commands can write only inside the evaluation workspace; other exposed paths are read-only. | Denied unless `--network=true`.                 |
| `read-only`   | Commands cannot write to the workspace or host.                                             | Denied unless `--network=true`.                 |
| `unsandboxed` | No filesystem guarantee.                                                                    | No egress guarantee; requires `--network=true`. |

### Host tools

Runs see a fixed system `PATH`, so an evaluation does not inherit whatever happens to be installed on the machine. A skill whose subject is a tool installed outside those directories cannot be measured under that default: the agent reports the tool as unavailable and loses the comparison to a baseline that improvises, which grades as a skill failure while the real cause is the boundary.

`--allow-tool <name>` names an executable the run needs. Each name is resolved on the host before the workspace exists; a name that does not resolve refuses the policy rather than running without it, because a silently absent tool is indistinguishable from a skill that does not work. Resolved tools join `PATH` and are granted read permission in the adapter profile.

Declared tools are part of the policy, so they change the recorded policy digest and appear in the manifest. A run with a tool exposed is not the same boundary as a sealed one and is not presented as such. Judges never receive them: they read copied artifacts and need nothing from the host.

### Credential boundary

`credential_boundary: enforced` means model-generated commands cannot read source or temporary agent credentials. `none` makes no such claim. Protected levels require `enforced`; `unsandboxed` records `none` and requires `SHUHARI_I_UNDERSTAND_NO_CREDENTIAL_BOUNDARY=1`.

All modes strip known GitHub credential variables from child commands. This is only a mitigation in `unsandboxed`; same-UID commands can still read credential files.

### Adapter resolution and refusal

An explicit `--sandbox` wins over `SHUHARI_SANDBOX`. `ResolveSecurity` returns one digest-bearing adapter mapping; the engine validates and reuses it for runs and judge workspaces. `Probe` checks the native sandbox before workspace creation. Unsupported mappings or hosts return `ErrUnsupportedSecurityPolicy`; Shuhari never degrades.

Judges use resolved `read-only` without network for protected execution. When
the evaluated execution is explicitly acknowledged `unsandboxed`, the judge
reuses that unsandboxed resolution as well; its per-side copied artifact tree
still gives each judge only the files for its blinded side. An unsandboxed
judge may nevertheless access paths outside that copied tree, so no kernel
filesystem guarantee is claimed; evaluators must record and review that weaker
boundary. Artifacts are not rendered into the judge prompt. The comparator
remains prompt-rendered because it compares two outputs rather than grounding
a positive claim.

### Security provenance

Schema-v2 manifests let operators verify the exact security boundary and prompt identity used for a run: they record the neutral level, network access, credential boundary, adapter, native mode, policy digest, `judge_security`, and grader/comparator prompt digests. Trigger verdicts use schema v3.

## Codex adapter

| Shuhari level | Codex implementation                                        | Recorded credential boundary |
| ------------- | ----------------------------------------------------------- | ---------------------------- |
| `isolated`    | Codex `workspace-write` plus the Shuhari permission profile | `enforced`                   |
| `read-only`   | Codex `read-only` plus the Shuhari permission profile       | `enforced`                   |
| `unsandboxed` | Codex `danger-full-access`                                  | `none`                       |

Protected runs give the Codex client a private temporary home while child commands receive a minimal environment that denies source and temporary Codex homes. Claude Code and Gemini CLI remain unsupported until their adapters pass conformance.

Codex cancels and retries an attempt with no model item after 90 seconds. Set `SHUHARI_FIRST_TOKEN_TIMEOUT` to a longer positive duration only when first items routinely take longer; after the first item, only the normal run timeout applies.

## Adapter contract

- `Capabilities` declares skill, instructions, and trigger-evidence support.
- `ResolveSecurity` returns an exact mapping or `ErrUnsupportedSecurityPolicy`.
- `Probe` verifies the supplied resolutions on the host before work begins.
- `Run` starts a clean context and returns response, transcript, usage, timing, read evidence, and actions.

The adapter owns native flags, state isolation, retries, event parsing, and conformance proof. Repository gate policy stays outside it.

## Adapter conformance

An adapter is selectable only after tests prove:

- stable mappings, typed refusals, host preflight, and unchanged resolution reuse;
- workspace and outside-path reads/writes, symlink escapes, and source/temporary credential denial;
- denied and allowed network behavior;
- native event parsing and explicit `unsandboxed` provenance.

CI runs these probes against the real Codex child sandbox for both protected levels and both network states. Credential, filesystem, and network mutations must make the suite fail.

## Trigger evidence

Trigger checks retain ordered transcript evidence for `target_read` and record
the separate judge decision as `target_applied`. A positive application case
passes only when both the read and application policies are satisfied; a
negative control must not apply the skill. The trigger subsystem keeps its
mechanical read coverage and blind application verdict independently of
evaluation grading.

## Documentation growth

Keep development documents flat under `docs/` while each subject fits in one page. Expected siblings include schema references, [eval-authoring guidance](eval-authoring.md), integration boundaries, and the release process. Create a subject subdirectory only when one of those topics grows into multiple documents; do not add empty category directories in advance.
