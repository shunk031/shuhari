# Shuhari

Shuhari (守破離) evaluates skills and persistent instructions against real coding agents. It follows the [Agent Skills evaluation workflow](https://agentskills.io/skill-creation/evaluating-skills): run each case in a clean context with and without the guidance, grade assertions with evidence, and retain outputs, timing, and aggregate statistics in an iteration workspace.

The initial release provides three commands:

```text
shuhari eval skill <skill-path-or-file>...
shuhari eval instructions <instructions-file>
shuhari check trigger <skill-path>
```

`eval skill` and `eval instructions` measure output quality. `check trigger` is a separate gate-oriented check for whether a skill is read for relevant prompts and ignored for near-misses.

## Install

Install a release from GitHub with the [mise GitHub backend](https://mise.jdx.dev/dev-tools/backends/github.html):

```sh
mise use -g github:shunk031/shuhari
```

Or install from source:

```sh
go install github.com/shunk031/shuhari/cmd/shuhari@latest
```

Shuhari currently requires an authenticated [Codex CLI](https://developers.openai.com/codex/noninteractive/) on `PATH`. It reuses the CLI's authentication and configuration; Shuhari does not manage credentials, providers, or gateway settings.

## Evaluate a skill

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

`prompt`, `expected_output`, and `files` follow the Agent Skills guide. `assertions` can be added after inspecting the first run; when omitted, Shuhari grades the human-readable `expected_output`. File paths are relative to the skill root and cannot escape it.

Run the evaluation:

```sh
shuhari eval skill path/to/csv-analyzer
```

Each case runs in fresh temporary Git repositories. The candidate has the skill installed; the baseline does not. An assertion grader evaluates both output artifacts. A separate blind comparator receives the original task and chooses the more useful result without knowing which variant it represents. Shuhari writes an iteration beside the skill by default:

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

With multiple trials, trial 1 occupies the canonical path above and later trials are stored under `trials/<N>/`. `benchmark.json` contains pass-rate, time, and token statistics, with-minus-without deltas, and deterministic assertion audit categories. `manifest.json` records schema version, input and runner digests, agent identity, effective settings, and both judge-prompt digests. Checked-in JSON Schemas under [`schemas/`](schemas/) define the input and durable artifact contracts.

## Evaluate instructions

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

```sh
shuhari eval instructions path/to/AGENTS.md
```

Use `--evals <path>` when the eval file is elsewhere. The workspace uses `with_instructions` and `without_instructions` variants.

## Check skill triggers

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

## Gate policy

Shuhari implements the portable mechanism. Each consuming repository owns file selection and policy values such as trials, concurrency, timeout, network access, and pre-commit `SKIP` behavior. Keep those values explicit in the hook:

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

The default sandbox is `workspace-write`. Enable network access only for cases that need it:

```sh
shuhari eval skill path/to/skill --network
```

On hosts where the Codex sandbox cannot start but the surrounding environment already provides isolation, override it explicitly:

```sh
SHUHARI_SANDBOX=danger-full-access shuhari eval skill path/to/skill
```

`required_actions` is an optional Shuhari extension for cases that must observe `web_search`, `github_search`, or `file_change` in order. It checks successful trace evidence, requires the ordered actions in every trial, and never enables network access implicitly.

Successful runs are cached by the target contents and filenames, effective policy, agent executable/configuration/environment identity, both judge prompts, and Shuhari binary. Failed runs are not cached; their iteration remains available with machine-readable error evidence and completed per-run artifacts. Use `--no-cache` for a fresh run.

Exit status is `0` for a pass, `1` for an evaluation or trigger-policy failure, and `2` for invalid input or an execution error.

## Agent adapters

Codex is the only adapter in the initial release. The execution boundary is intentionally small: an adapter reports capabilities and identity, runs one isolated request, and returns a response, transcript, usage, target-read evidence, and observed actions. Claude Code or Antigravity support can implement that boundary later without changing eval schemas or workspace artifacts.

See [ARCHITECTURE.md](ARCHITECTURE.md) for package responsibilities.
