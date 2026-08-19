# Eval Authoring

Write cases that isolate one observable behavior and provide every input they need.

## Trigger cases

- Positive cases use realistic, in-domain prompts that should lead the agent to apply the skill; consultation without application does not count.
- A `should_trigger: false` case measures application frequency by trial majority; reading and explicitly declining an out-of-scope skill is a non-trigger.
- Negative controls are near-misses that share the skill's keywords or concepts; an obviously irrelevant prompt tests nothing.

## Evaluation cases

- Make each scenario self-contained: supply working material through `files` fixtures and never assume a populated checkout.
- Put one requirement in each assertion, and judge produced behavior or artifacts rather than the presence or absence of literal prose.
- Do not require long verbatim tokens such as full hashes or URLs; give the prompt a short form when exact transcription is not the behavior under test.
- To test failure handling or another conditional policy, ask what happens under that condition or make the condition occur; do not require recitation of an unobserved counterfactual.
- Write each assertion so its text fully determines the intended judgment; if graders repeatedly infer a stricter requirement, fix the assertion.

## Evidence

- Judges record concise free-form evidence, such as a quote or file reference. Shuhari stores it as returned; deterministic checks belong in verification scripts.
