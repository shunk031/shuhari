# Grading contract

This is the canonical contract for evaluation authors, judge-prompt authors,
harness implementers, and gate operators. It follows the Agent Skills
evaluation workflow: run each case with and without the target guidance, grade
both outputs, compare them blindly, and retain the receipts.

## Certification outcome

Execution and judge invocation failures are fatal only when setup, security,
transport, timeout, or output structure prevents grading. A valid `passed:false`
result is retained as a side-specific failed assertion. Trial policy, benchmark
aggregation, and blind comparison determine the evaluation result. Trigger
certification separately requires `target_read` to show that the target was
read and `target_applied` to show that the application judge saw the skill
applied; each must match its case's `should_trigger` policy.

## Judge inputs and blindness

For each case and trial, Shuhari deterministically maps the two configurations
to blind labels. It invokes one grader agent per side with only:

```json
{
  "id": "case-id",
  "trial": 1,
  "side": "A",
  "assertions": ["assertion text"]
}
```

The grader's current working directory contains a copied artifact tree for that
side. It does not contain the other side, a skill name, or a candidate/baseline
identity. The grader reads files itself; artifacts are not rendered into its
prompt. The resolved `judge_security` policy is recorded in `manifest.json`.
An acknowledged unsandboxed resolution keeps the blinded copied tree but does
not provide a kernel boundary.

## Grading output

The grader returns one result for every assertion:

```json
{
  "text": "assertion text",
  "passed": true,
  "evidence": "free-form quote or file reference"
}
```

`text` must match the requested assertion, `passed` is the judge's decision,
and `evidence` is non-empty free-form supporting material. Shuhari records the
evidence as returned. It does not match quotes, normalize text, calculate a
grounding score, require line spans, or reinterpret evidence. A judge-failed
assertion is an ordinary failed assertion and continues through aggregation.
Malformed JSON or a structurally incomplete result fails the grading call
closed; it does not turn a valid failed verdict into an invocation failure.

The checked-in grading artifact uses the reference shape:

```json
{
  "expectations": [{"text": "...", "passed": true, "evidence": "..."}],
  "summary": {"passed": 1, "failed": 0, "total": 1, "pass_rate": 1.0}
}
```

## Assertions and absence

Assertions are author-owned text strings. Positive, negative, and mixed claims
are judged by the agent from the artifact. There is no special absence object,
clause-linking rule, forbidden-pattern field, contradiction resolver, or
evidence attachment matrix in the grading contract. A deterministic check that
an eval author needs belongs in a verification script, as in the reference
workflow, rather than in judge evidence parsing.

## Retry and receipts

The harness owns transport retry, first-response watchdog, timeout, and attempt
receipts for both grader and comparator calls. Completed responses are not
retried as transport failures. The grader's read-only/unsandboxed resolution is
passed through unchanged. Shuhari retains raw judge responses and
`judge-retries.json` when transport attempts occur; malformed structured output
is recorded in the grading error receipt and fails closed.

## Comparator

The comparator remains prompt-rendered because it compares two outputs and
does not claim that evidence text came from one artifact. It receives blinded
`A` and `B` artifact views plus the raw responses and returns one of `A`, `B`, or
`tie` with a non-empty reason. Missing cases, invalid preferences, blank
reasons, malformed JSON, and transport exhaustion fail closed. Comparator agent
conversion is out of scope for this simplification.

## Simplification lineage

This contract closes the grounding-fragility lineage. Earlier patches handled
inline-code and Markdown rendering, paraphrased and nested observations, then
absence schemas, relevance, and contradictions. The final verbatim-misquote
failure showed that prompt-rendered copying was the wrong boundary. The
judge-as-agent proposal in issue #13 supplied the structural cure: agents read
artifacts in a blinded workspace and evidence remains free-form. The old
matcher and matrix receipts are retired.
