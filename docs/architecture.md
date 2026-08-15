# Development Architecture Contract

Shuhari separates the Agent Skills evaluation mechanism from repository-specific gate policy and agent-specific runtime behavior. Maintainers should use these boundaries to decide whether new behavior belongs in Shuhari, a consuming repository, or an agent adapter.

## Responsibility boundaries

Shuhari owns:

- strict eval, instruction, and trigger schemas;
- clean per-trial workspaces and fixture staging;
- agent invocation through a narrow harness;
- blind assertion grading and A/B comparison;
- durable output, transcript, timing, grading, benchmark, and failure artifacts;
- per-case trial aggregation, strict negative controls, and success-only caching.

The consuming repository owns:

- which changed files select a skill or instructions target;
- pre-commit hook definitions and `SKIP` behavior;
- declared policy values such as `--trials`, `--jobs`, `--timeout`, `--network`, and `--strict-all-trials`;
- static checks for repository-specific naming, migration, or ownership rules.

The selected agent CLI owns authentication, model configuration, sandbox enforcement, and its native event format. Shuhari passes explicit overrides when requested and converts native events into its internal result contract.

Two integration boundaries remain open. The staged-target wrapper that maps pre-commit changes to Shuhari targets belongs to each consuming repository. Document slop review cannot import Go internals, and its runner and subprocess boundary is not yet specified. Authoring calibration loops, viewers, and servers remain separate tools outside Shuhari.

## Packages

```text
cmd/shuhari
    └── internal/cli
        ├── internal/eval
        │   ├── internal/skill
        │   ├── internal/harness
        │   └── internal/cache
        └── internal/trigger
            ├── internal/skill
            ├── internal/harness
            └── internal/cache
```

- `cmd/shuhari` is the executable entry point and receives the release version through linker flags.
- `internal/cli` defines only the public command tree, flags, output, and exit-code mapping.
- `internal/skill` reads and validates shared `SKILL.md` metadata.
- `internal/eval` loads skill or instructions cases, stages fixtures, runs candidate/baseline pairs, grades them, aggregates trials, and writes the Agent Skills workspace.
- `internal/trigger` loads near-miss and positive cases, measures target reads, applies trigger policy, and writes trigger evidence.
- `internal/harness` defines the agent-neutral request/result boundary and contains the Codex adapter. It gives the Codex client an isolated `CODEX_HOME`, gives model-generated commands a separate minimal environment, invokes `codex exec --ephemeral --json`, and parses structured events.
- `internal/cache` stores only successful results. Cache keys include the runner binary so evaluator changes invalidate prior results.

There is no public Go SDK or dynamic plugin registry. A new agent is added as a concrete harness implementation only when its CLI can provide the required isolation and evidence. The CLI schemas, workspace layout, and repository policy remain independent of that adapter.

## Evaluation flow

1. The CLI resolves and strictly validates the target and its cases.
2. The engine creates a new `iteration-N` directory and schedules a bounded number of candidate/baseline runs.
3. Every run receives a fresh temporary Git repository and isolated agent home. Fixtures are copied into the repository; evaluator-only fields such as `expected_output` are not included in the run prompt. Produced files and the final response are copied into the durable workspace.
4. A structured grader receives blinded A/B artifacts. Its response is checked for complete case, trial, assertion, and quoted-evidence coverage before grades are accepted.
5. A separate blind comparator receives the original task and both artifacts. Its unblinded mapping and decision are stored independently from assertion grades.
6. Candidate assertion results are aggregated per case. The default is majority; `--strict-all-trials` requires all trials. Required actions always require every trial. The comparison also requires candidate wins to be at least baseline wins.
7. `benchmark.json` records candidate-minus-baseline differences and assertion audit categories. A passing result enters the cache; a failure retains evidence but never enters the cache.

Trigger checks use the same isolation and bounded scheduling, but not the output grader. A skill read is observed only when a successful pre-response command emits the complete `SKILL.md` body; command names and path mentions are not evidence. Positive cases use majority by default, while negative controls always require zero reads.

## Credential boundary

The Codex client needs authentication while model-generated commands must not receive it. Shuhari therefore separates the two environments. The client process receives a temporary `CODEX_HOME` containing mode-0600 configuration and, when present, authentication files. Its generated profile sets command environment inheritance to `none`, supplies a synthetic `HOME` and curated command `PATH`, and sets command-side `CODEX_HOME` to an empty value.

For `workspace-write` and `read-only`, Shuhari selects a custom permission profile. It grants commands only the minimal runtime paths and the evaluation workspace, and explicitly denies both the source and temporary Codex home. This is the supported credential-isolation boundary.

`danger-full-access` disables Codex filesystem and network sandboxing. A same-UID command can search host paths even though direct variables such as `HOME` and `CODEX_HOME` are removed from its environment. Shuhari cannot provide strict credential isolation in that mode. Use it only inside an external container or equivalent host boundary that contains no credentials beyond those needed by the Codex client; `--network=false` is not an egress guarantee in this mode.

## Action evidence

Native successful file-change events retain their trace order. Shuhari also compares the workspace before and after the run so successful shell writes such as `cp` and redirection satisfy `file_change`; this fallback observation is placed after trace-observed actions. Standard `gh api`, `gh search`, `gh repo`, and `gh browse` commands satisfy `github_search` without requiring a literal GitHub URL.

## Documentation growth

Keep development documents flat under `docs/` while each subject fits in one page. Expected siblings include schema references, eval-authoring guidance, integration boundaries, and the release process. Create a subject subdirectory only when one of those topics grows into multiple documents; do not add empty category directories in advance.
