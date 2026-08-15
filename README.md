# Shuhari

[`Shuhari` (守破離)](https://en.wikipedia.org/wiki/Shuhari) evaluates skills and persistent instructions against real coding agents. It follows the [Agent Skills evaluation workflow](https://agentskills.io/skill-creation/evaluating-skills): run each case in a clean context with and without the guidance, grade assertions with evidence, and retain outputs, timing, and aggregate statistics in an iteration workspace.

## Install

### GitHub Releases

Tagged releases publish prebuilt archives for Linux, macOS, and Windows on the [GitHub Releases page](https://github.com/shunk031/shuhari/releases). Download the archive for your operating system and architecture, extract `shuhari`, and place it on your `PATH`.

### Go

Install from source with Go:

```sh
go install github.com/shunk031/shuhari/cmd/shuhari@latest
```

Make sure the Go binary directory, normally `$(go env GOPATH)/bin`, is on your `PATH`.

### mise

Install a GitHub release with the [mise GitHub backend](https://mise.jdx.dev/dev-tools/backends/github.html):

```sh
mise use -g github:shunk031/shuhari
```

The initial release runs evaluations through the [Codex CLI](https://developers.openai.com/codex/noninteractive/). Install `codex`, put it on your `PATH`, and sign in with `codex login` before running Shuhari. Shuhari calls that local CLI and uses its existing login and settings; it does not install Codex or accept or store your login credentials.

## How to Use

Shuhari groups output evaluation and trigger checks as sibling commands:

- `shuhari eval` compares results with and without guidance.
  - `shuhari eval skill <skill-path-or-file>...`
  - `shuhari eval instructions <instructions-file>`
- `shuhari check` measures gate-oriented behavior.
  - `shuhari check trigger <skill-path>`

### Prepare a skill evaluation

Place `evals/evals.json` inside the skill directory:

```json
{
  "skill_name": "csv-analyzer",
  "evals": [
    {
      "id": 1,
      "prompt": "Find the top three months by revenue and make a bar chart.",
      "expected_output": "A labeled bar chart with the three highest-revenue months.",
      "files": ["evals/files/sales.csv"],
      "assertions": [
        "The output identifies the correct three months.",
        "The chart has labeled axes."
      ]
    }
  ]
}
```

`prompt`, `expected_output`, and `files` follow the Agent Skills guide. Add `assertions` after inspecting the first run; when they are omitted, Shuhari grades the human-readable `expected_output`. File paths are relative to the skill root and cannot escape it.

### Evaluate a skill

Run the evaluation:

```sh
shuhari eval skill path/to/csv-analyzer
```

Each case runs in fresh temporary Git repositories. The candidate has the skill installed; the baseline does not. An assertion grader evaluates both output artifacts. A separate blind comparator receives the original task and chooses the more useful result without knowing which variant it represents.

By default, Shuhari writes an iteration workspace beside the skill:

```text
csv-analyzer-workspace/
└── iteration-1/
    ├── eval-1/
    │   ├── comparison.json
    │   ├── with_skill/
    │   │   ├── outputs/
    │   │   ├── transcript.jsonl
    │   │   ├── timing.json
    │   │   └── grading.json
    │   └── without_skill/
    │       └── ...
    ├── benchmark.json
    └── manifest.json
```

With multiple trials, trial 1 occupies the canonical path above and later trials are stored under `trials/<N>/`.

- `benchmark.json` contains pass-rate, time, and token statistics, with-minus-without deltas, and deterministic assertion audit categories.
- `manifest.json` lets you check whether the inputs, runner, agent settings, or judge prompts changed before comparing two iterations.
- Checked-in JSON Schemas under [`schemas/`](schemas/) define the input and durable artifact contracts.

### Prepare an instructions evaluation

Instructions use the same run, grade, and aggregate flow. By default, `AGENTS.md` is paired with `AGENTS.evals.json` in the same directory:

```json
{
  "instructions_name": "project-guidance",
  "evals": [
    {
      "id": "verification",
      "prompt": "Implement the requested change.",
      "expected_output": "The response reports the relevant verification.",
      "assertions": ["The response identifies the verification that was run."]
    }
  ]
}
```

Use `--evals <path>` when the eval file is elsewhere.

### Evaluate instructions

Run the evaluation:

```sh
shuhari eval instructions path/to/AGENTS.md
```

The workspace uses `with_instructions` and `without_instructions` variants.

### Check skill triggers

Trigger cases are deliberately separate from output-quality evals. Store them in `evals/triggers.json`:

```json
{
  "skill_name": "csv-analyzer",
  "cases": [
    {
      "id": "relevant",
      "prompt": "Analyze this sales CSV and chart the result.",
      "should_trigger": true
    },
    {
      "id": "near-miss",
      "prompt": "Explain what comma-separated values are.",
      "should_trigger": false
    }
  ]
}
```

Both positive cases and negative controls are required. Positive cases pass by per-case trial majority; a negative control fails if the skill is read even once. `--strict-all-trials` also requires every positive trial to trigger.

```sh
shuhari check trigger path/to/csv-analyzer --trials 3 --jobs 2 --timeout 600
```

### Add a repository gate

Shuhari implements the portable evaluation mechanism. Each consuming repository owns file selection and policy values such as trials, concurrency, timeout, network access, and pre-commit `SKIP` behavior. Keep those values explicit in the hook:

```yaml
- repo: local
  hooks:
    - id: skill-eval
      name: evaluate changed skills
      entry: shuhari eval skill --trials 3 --jobs 2 --timeout 600
      language: system
      files: ^skills/.+/(SKILL\.md|evals/.+)$
      pass_filenames: true

    - id: skill-trigger
      name: check skill triggers
      entry: shuhari check trigger skills/csv-analyzer --trials 3 --jobs 2 --timeout 600
      language: system
      files: ^skills/csv-analyzer/(SKILL\.md|evals/triggers\.json)$
      pass_filenames: false
```

Changed files passed to `eval skill` are resolved to the nearest `SKILL.md` and deduplicated. Use `--validate-only` for a fast schema and fixture check that does not start an agent.

### Configure evaluation runs

The default sandbox is `workspace-write`. Enable network access only for cases that need it:

```sh
shuhari eval skill path/to/skill --network
```

On hosts where the Codex sandbox cannot start but the surrounding environment already provides isolation, override it explicitly:

```sh
SHUHARI_SANDBOX=danger-full-access shuhari eval skill path/to/skill
```

This override removes Codex's command filesystem and network boundary. Shuhari still removes credential paths from the generated command environment, but same-UID commands can search the host filesystem. Use the override only inside an external container or equivalent credential-free boundary. See the [credential boundary](docs/architecture.md#credential-boundary).

`required_actions` is an optional Shuhari extension for cases that must observe `web_search`, `github_search`, or `file_change` in order. It checks successful trace evidence and workspace effects, requires the ordered actions in every trial, and never enables network access implicitly.

Successful runs are cached by the target contents and filenames, effective policy, agent executable/configuration/environment identity, both judge prompts, and Shuhari binary. Failed runs are not cached; their iteration remains available with machine-readable error evidence and completed per-run artifacts. Use `--no-cache` for a fresh run.

Exit status is `0` for a pass, `1` for an evaluation or trigger-policy failure, and `2` for invalid input or an execution error.

Codex is the only adapter in the initial release. Other coding-agent CLIs can implement the same narrow execution boundary without changing eval schemas or workspace artifacts. See the [development architecture contract](docs/architecture.md) for package responsibilities and extension boundaries.
