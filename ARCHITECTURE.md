# Architecture

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

The selected agent CLI owns authentication, model/provider configuration, sandbox enforcement, and its native event format. Shuhari passes explicit overrides when requested and converts native events into its internal result contract.

The former dotfiles evaluator responsibilities map to these boundaries as follows:

| Responsibility | Owner |
| --- | --- |
| Agent Skills-compatible cases, fixtures, A/B runs, grading, aggregation, artifacts | Shuhari `eval` |
| Instructions A/B runs using the same evaluation contract | Shuhari `eval instructions` |
| Skill-read measurement, positive majority, strict negative controls | Shuhari `check trigger` |
| Trial isolation, bounded concurrency, retry, cache, and failure evidence | Shuhari core |
| Changed-file and staged-target discovery | Consuming repository |
| Pre-commit globs, policy flag values, and `SKIP` behavior | Consuming repository |
| Authentication, provider selection, and sandbox implementation | Agent CLI |
| Document slop review, authoring calibration loops, viewers, and servers | Separate tools, outside Shuhari |

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
- `internal/harness` defines the agent-neutral request/result boundary and contains the Codex adapter. It also isolates `CODEX_HOME`, passes a minimal allowlisted environment, invokes `codex exec --ephemeral --json`, and parses structured events.
- `internal/cache` stores only successful results. Cache keys include the runner binary so evaluator changes invalidate prior results.

There is no public Go SDK or dynamic plugin registry. A new agent is added as a concrete harness implementation only when its CLI can provide the required isolation and evidence. The CLI schemas, workspace layout, and repository policy remain independent of that adapter.

## Evaluation flow

1. The CLI resolves and strictly validates the target and its cases.
2. The engine creates a new `iteration-N` directory and schedules a bounded number of candidate/baseline runs.
3. Every run receives a fresh temporary Git repository and isolated agent home. Fixtures are copied into the repository; produced files and the final response are copied into the durable workspace.
4. A structured grader receives blinded A/B artifacts. Its response is checked for complete case, trial, assertion, and quoted-evidence coverage before grades are accepted.
5. A separate blind comparator receives the original task and both artifacts. Its unblinded mapping and decision are stored independently from assertion grades.
6. Candidate assertion results are aggregated per case. The default is majority; `--strict-all-trials` requires all trials. Required actions always require every trial. The comparison also requires candidate wins to be at least baseline wins.
7. `benchmark.json` records candidate-minus-baseline differences and assertion audit categories. A passing result enters the cache; a failure retains evidence but never enters the cache.

Trigger checks use the same isolation and bounded scheduling, but not the output grader. They measure whether the agent read the installed skill. Positive cases use majority by default, while negative controls always require zero reads.

## Integration boundary

The staged-target wrapper that maps pre-commit changes to Shuhari targets remains a consuming-repository follow-up. Document slop review cannot import Go internals; its runner and subprocess boundary is also an unspecified follow-up.
