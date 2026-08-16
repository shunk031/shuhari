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
2. The engine asks the selected adapter to resolve the requested security policy. The exact resolution is validated once and then passed unchanged to run requests, artifacts, and cache-key construction. Evaluation judges receive a separately resolved `read-only`, offline policy.
3. The engine creates a new `iteration-N` directory and schedules a bounded number of candidate/baseline runs.
4. Every run receives a fresh temporary Git repository and isolated agent home. Fixtures are copied into the repository; evaluator-only fields such as `expected_output` are not included in the run prompt. Produced files and the final response are copied into the durable workspace.
5. A structured grader receives blinded A/B artifacts. Its response is checked for complete case, trial, assertion, and quoted-evidence coverage before grades are accepted.
6. A separate blind comparator receives the original task and both artifacts. Its unblinded mapping and decision are stored independently from assertion grades.
7. Candidate assertion results are aggregated per case. The default is majority; `--strict-all-trials` requires all trials. Required actions always require every trial. The comparison also requires candidate wins to be at least baseline wins.
8. `benchmark.json` records candidate-minus-baseline differences and assertion audit categories. A passing result enters the cache; a failure retains evidence but never enters the cache.

Passing evidence is `strong` when its normalized quote occurs in the artifact. Otherwise, a quote of at least eight tokens is a grounded `paraphrase` when its token LCS recall is at least 0.75 within an artifact window no longer than twice the quote; `grading.json` records the score and best-matching normalized artifact span. Evidence below either bar is a `hallucination` and fails closed.

Trigger checks measure whether the agent reads the target `SKILL.md`; they do not grade output quality. Each trial uses the same isolation and bounded scheduling as an evaluation run. Read evidence accumulates only from successful pre-response commands that reference the target. The body is split into nonblank, byte-weighted chunks of at most 128 bytes; a read requires at least 90% cumulative coverage. This tolerates a truncated tail while rejecting metadata-only commands and materially incomplete reads. A case passes when at least `floor(trials/2)+1` outcomes match `should_trigger`; `--strict-all-trials` raises that requirement to every trial.

## Execution security contract

Shuhari exposes its own security vocabulary. Native agent mode names are adapter implementation details and must not appear as accepted `--sandbox` or `SHUHARI_SANDBOX` values.

### Neutral sandbox levels

| Level | Filesystem guarantee | Network guarantee |
| --- | --- | --- |
| `isolated` | Evaluated commands may write only inside the evaluation workspace. Outside it, the adapter exposes only the minimum read-only runtime paths needed to execute commands. | Denied by default; allowed only when `--network=true` is requested and the adapter can enforce that choice. |
| `read-only` | Evaluated commands cannot write to the evaluation workspace or host paths. The adapter exposes only the minimum read-only runtime paths needed to execute commands. | Denied by default; allowed only when `--network=true` is requested and the adapter can enforce that choice. |
| `unsandboxed` | Shuhari makes no filesystem restriction claim. | Shuhari makes no egress restriction claim, so this level is accepted only with explicit `--network=true`. |

An adapter must implement these guarantees literally. A mode that is merely similar is not an acceptable mapping.

### Credential boundary

Credential protection is recorded separately from the sandbox level:

- `enforced` means model-generated commands cannot read either the caller's source authentication material or the temporary authentication material used by the agent client.
- `none` means Shuhari provides no credential-separation guarantee. An outer container or runner may still provide one, but that protection is outside the artifact claim.

`isolated` and `read-only` require `credential_boundary: enforced`. `unsandboxed` records `credential_boundary: none` and requires `SHUHARI_I_UNDERSTAND_NO_CREDENTIAL_BOUNDARY=1`. Keeping this field separate prevents a sandbox label from silently standing in for a credential guarantee.

### Adapter resolution and refusal

The harness method `ResolveSecurity(context.Context, SecurityPolicy) (SecurityResolution, error)` maps a requested neutral policy to one exact adapter resolution. A resolution contains the neutral level, effective network access, credential boundary, adapter name, native mode, and a digest of the adapter policy.

The engine validates the resolution against the request before creating artifacts. It passes that same value to every corresponding `Run` call; `Run` validates the supplied adapter resolution but does not resolve the policy again. If an adapter cannot honor any requested guarantee, it returns `ErrUnsupportedSecurityPolicy`. Shuhari does not degrade to a weaker mode or relabel a weaker result as compliant.

### Security provenance

Workspace manifests, `benchmark.json`, and `trigger.json` use schema version 2 and record a `security` object:

```json
{
  "sandbox_level": "isolated",
  "network_access": "denied",
  "credential_boundary": "enforced",
  "adapter": {
    "name": "codex",
    "native_mode": "workspace-write+shuhari-permission-profile",
    "policy_digest": "sha256:..."
  }
}
```

The v2 cache namespace and cache key include the requested configuration, resolved run security, resolved judge security, adapter identity, runner binary, suite inputs, and judge prompt digests. Changing a neutral level, native mapping, or adapter-policy implementation cannot reuse a v1 or differently secured success.

## Codex mapping

The Codex adapter is the current implementation example:

| Shuhari level | Codex implementation | Recorded credential boundary |
| --- | --- | --- |
| `isolated` | Codex `workspace-write` plus the Shuhari permission profile | `enforced` |
| `read-only` | Codex `read-only` plus the Shuhari permission profile | `enforced` |
| `unsandboxed` | Codex `danger-full-access` | `none` |

For the two enforced modes, the Codex client receives a randomized mode-0700 temporary tree with mode-0600 configuration and authentication files. Model-generated commands receive a separate minimal environment, and the permission profile explicitly denies both the source and temporary Codex homes. The `unsandboxed` mapping cannot enforce either filesystem isolation or network denial, so `unsandboxed` with `--network=false` is rejected.

Claude Code and Gemini CLI mappings are not implemented. Selecting an unavailable agent or a policy without a verified mapping fails closed.

## Adapter contract

A harness implementation must provide:

- `Probe`, returning the executable, version, configuration, and environment identity used for provenance and caching;
- `Capabilities`, declaring whether skill installation, instructions installation, and trigger-read evidence are supported;
- `ResolveSecurity`, returning a validated, digest-bearing mapping for a neutral policy or `ErrUnsupportedSecurityPolicy` without degradation;
- `Run`, starting a clean agent context with the supplied resolution, timeout, model settings, workspace, optional target, and optional structured-output schema;
- structured result data containing the final response, raw transcript, token usage, duration, target-read evidence, ordered actions, and explicitly order-unknown actions.

The adapter owns native invocation flags, isolated agent state, retry cleanup, event parsing, and the proof that its recorded native mode enforces the neutral contract. It must not reinterpret repository gate policy such as trial counts or strict negative controls.

## Adapter conformance

Every new adapter must pass a platform-appropriate conformance suite before it is selectable:

- resolve every supported neutral level to stable adapter metadata and reject every unsupported policy with `ErrUnsupportedSecurityPolicy`;
- exercise workspace reads and writes, outside-workspace reads and writes, and symlink escapes against the effective child-command sandbox;
- attempt to read both source and temporary authentication material from a model-command child;
- exercise denied and allowed network policies rather than inspecting generated configuration text;
- fail closed when the native sandbox is unavailable;
- prove that a supplied resolution is used unchanged and that a mutated level, native mode, or policy digest is rejected;
- parse recorded native event fixtures for completion, failures, usage, target reads, and action ordering;
- test acknowledged `unsandboxed` execution separately and prove that its artifacts record `credential_boundary: none` and `network_access: allowed`.

The Codex conformance job runs the real child sandbox for `isolated` and `read-only`. A mutation run removes the credential-deny entries and must fail, proving that the regression test depends on the effective policy rather than matching configuration text.

## Action evidence

Native successful actions retain their trace order. Shuhari also compares the workspace before and after the run so shell writes such as `cp` and redirection satisfy `file_change`. Because a final workspace diff cannot identify which command made the change, that evidence is marked order-unknown rather than appended to the trace. Required-action matching accepts it in any slot that is consistent with the known trace order. This proves that a file change occurred, but not whether it happened before or after another action; cases that require that exact relationship need native ordered evidence. Standard `gh api`, `gh search`, `gh repo`, and `gh browse` commands satisfy `github_search` without requiring a literal GitHub URL.

## Documentation growth

Keep development documents flat under `docs/` while each subject fits in one page. Expected siblings include schema references, [eval-authoring guidance](eval-authoring.md), integration boundaries, and the release process. Create a subject subdirectory only when one of those topics grows into multiple documents; do not add empty category directories in advance.
