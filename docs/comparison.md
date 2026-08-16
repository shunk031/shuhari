# Evaluation Framework Comparison

Maintainers choosing an evaluation tool can use this table to compare built-in behavior documented on 2026-08-16. It focuses on skill and context evaluators; Inspect AI represents general agent-evaluation frameworks. The table is maintained by Shuhari's authors and may be incomplete.

| Feature | Shuhari | Anthropic skill-creator | Microsoft Waza | NVIDIA SkillEvaluator | Tessl | Inspect AI |
| --- | :---: | :---: | :---: | :---: | :---: | :---: |
| Agent skills as first-class targets | ✅ | ✅ | ✅ | ✅ | ✅ | — |
| Repository guidance as a first-class target | ✅ | — | — | — | ✅ | — |
| Built-in with/without A/B runs | ✅ | ✅ | ✅ | ✅ | ✅ | — |
| Quoted artifact evidence required per assertion | ✅ | — | — | — | — | — |
| Blind A/B output comparison | ✅ | ✅ | — | — | — | — |
| Trigger checks with near-miss negatives | ✅ | ✅ | — | — | — | — |
| Majority decides positive cases; any negative trigger fails | ✅ | — | — | — | — | — |
| Fresh isolated workspace per trial or sample | ✅ | — | ✅ | ✅ | — | ✅ |
| Agent-independent sandbox policy with protected credentials | 🚧 | — | — | — | — | — |
| Cache reuses only successful runs with matching inputs and runtime | ✅ | — | — | — | — | — |
| Machine-readable eval definitions and results | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ |
| Repository gate integration | ✅ | — | ✅ | — | ✅ | — |
| Multiple coding-agent adapters | — | — | — | ✅ | ✅ | ✅ |

✅ means the reviewed sources document the feature. 🚧 means implementation is open but not merged. — means the reviewed sources do not establish the feature; it does not assert that the product cannot support it.

## Sources

- **Shuhari:** [architecture contract](architecture.md), [evaluation engine](../internal/eval/engine.go), [trigger checker](../internal/trigger/check.go), and [neutral sandbox PR](https://github.com/shunk031/shuhari/pull/2)
- **Anthropic skill-creator:** [workflow](https://github.com/anthropics/skills/blob/f6656c1256d5a8adfa37db9110046ef20bac644c/skills/skill-creator/SKILL.md), [grader](https://github.com/anthropics/skills/blob/f6656c1256d5a8adfa37db9110046ef20bac644c/skills/skill-creator/agents/grader.md), and [blind comparator](https://github.com/anthropics/skills/blob/f6656c1256d5a8adfa37db9110046ef20bac644c/skills/skill-creator/agents/comparator.md)
- **Microsoft Waza:** [documentation](https://microsoft.github.io/waza/) and [run command](https://github.com/microsoft/waza/blob/f3afa6aa61dd9bbbc893067c7e949672952f9cd0/cmd/waza/cmd_run.go)
- **NVIDIA SkillEvaluator:** [Tier 3 evaluator](https://github.com/NVIDIA/SkillEvaluator/blob/81478954fa3fced62ec72f3e3df8f0bde51076f7/src/skillevaluator/tier3/harbor/runner.py) and [agent adapters](https://github.com/NVIDIA/SkillEvaluator/blob/81478954fa3fced62ec72f3e3df8f0bde51076f7/src/skillevaluator/tier3/harbor/local_agents.py)
- **Tessl:** [skill evaluation](https://docs.tessl.io/improving-your-skills/evaluate-skill-quality-using-scenarios) and [CI gates](https://docs.tessl.io/codifying-and-enforcing-your-skill-standards/gate-skill-quality-in-ci)
- **Inspect AI:** [agent bridge](https://inspect.aisi.org.uk/agent-bridge.html), [sandboxing](https://inspect.aisi.org.uk/sandboxing.html), and [evaluation logs](https://inspect.aisi.org.uk/eval-logs.html)
