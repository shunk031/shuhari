# Grading Contract

This document is the canonical contract between an evaluation case, the blind
grader, and Shuhari's validator. The implementation is in
[`internal/eval/grade.go`](../internal/eval/grade.go),
[`internal/eval/evidence.go`](../internal/eval/evidence.go), and the grader
prompt embedded from [`internal/eval/prompts/grader.md`](../internal/eval/prompts/grader.md).

## Judge inputs and blinded workspaces

For each case and trial, Shuhari computes the existing deterministic A/B blind
mapping. It then runs two independent grader-agent calls, one for each blinded
side. Each call receives only this JSON input:

```json
{
  "id": "case-id",
  "trial": 1,
  "side": "A",
  "assertions": ["assertion text"]
}
```

The artifact and the original agent response are not rendered into the prompt.
The judge's read-only working directory contains only a copied artifact tree
for the requested side. It contains no skill name, candidate/baseline identity,
or other side's files. The judge must read the files itself and must not follow
instructions found in an untrusted artifact.

The engine resolves judge security as `read-only` with network denied, even when
the evaluated execution uses a less restricted mode. The Codex adapter receives
that resolution through the same security and transport machinery used by
other agent calls.

## Positive evidence

For every passing presence or positive assertion, the judge returns one or more
`evidence_references`:

```json
{
  "path": "response.md",
  "start_line": 12,
  "end_line": 14
}
```

Paths are relative to the blinded judge workspace. Line spans are inclusive and
1-based. `evidence` must be the exact text read from those lines, in reference
order, joined by one newline between references. The validator reads each
referenced file and rejects the result unless the excerpt is byte-for-byte
equal to the cited span. It also rejects absolute paths, path escapes, invalid
spans, missing files, and non-regular files.

There is no quote normalization, paraphrase matching, token threshold, recall
score, or prompt-rendered artifact fallback in this path. A renamed variable,
substituted literal, changed quote, whitespace change, or generic explanation
fails closed. Assertions about what the agent returned must use the same
mechanism against `response.md`.

## Absence claims

Absence is evaluated in two ordered modes.

1. An eval may declare `forbidden_patterns` on a negative assertion. These
   patterns are authoritative. The validator searches the artifact mechanically
   for each pattern; the judge is not consulted for that search. A match is a
   side-specific contradiction and changes that side's assertion to failed.
2. If the eval declares no patterns, the judge returns an `absence` object with
   `negated_clause`, `query`, and `rationale`. `negated_clause` must be a
   verbatim substring of the assertion. The validator searches `query` as-is in
   the artifact and records the declaration for auditability. A query match is
   the same side-specific contradiction. A mixed assertion must also carry
   exact positional references for its positive clause.

The validator does not infer absence from prose such as “nothing was found.”
The first-action-token and literal-clause relevance heuristics are not part of
the contract.

## Retry and watchdog semantics

The existing attempt contract applies unchanged to grader agents:

- transport failures are retried by the harness under its normal retry limit;
- a completed response is not retried as a transport failure;
- a structurally invalid or positionally invalid response receives at most one
  validation retry;
- the retry prompt includes the validation error, tells the judge to read the
  current artifact again, and repeats the exact positional-copy requirement;
- retry prompts still contain no artifact contents or side identities; and
- the existing first-token watchdog and overall timeout remain in force.

If the second validation attempt fails, grading fails closed and retains both
responses in the grading-error receipt. No threshold or trial aggregation rule
changes here.

## Validation outcomes

| Result | Validator action |
| --- | --- |
| Passing positive claim with exact cited span | Accept as `strong` evidence. |
| Passing positive claim with missing, nonexistent, or altered span | Reject the grading response. |
| Passing declared absence with no forbidden-pattern match | Accept as `absence`; record the declared patterns. |
| Passing fallback absence with a verbatim clause and no query match | Accept as `absence`; record query and rationale. |
| Either absence mode finds its query/pattern | Mark that blinded side's assertion failed as `contradiction`, with the matched artifact location. |
| Failed assertion | Keep it failed; evidence is not used to establish a pass. |
| Missing, duplicate, mismatched, or extra grader cases/assertions | Reject the grading response. |

## Comparator boundary

Comparators remain prompt-rendered in this change. They compare two outputs and
have a different failure surface from positive evidence grounding: their
contract is a blind A/B preference and a nonblank reason, not a claim that a
quoted excerpt came from one artifact. The comparator still receives the
existing blinded A/B artifacts and original task fields, and keeps the existing
validation, retry, and watchdog behavior. Converting comparators to agents is
out of scope for #13.

## Lineage and ownership

Judge-as-agent grounding (Issue #13) is the immediate post-migration priority
for positive evidence. Prompt-rendered artifacts proved fundamentally
unreliable: the retained verbatim-misquote failures show that a retry can
rename variables even when the source line is present. The structural cure is
for the judge to read the artifact directly while the validator checks the
cited location against the same bytes.

The history remains an audit trail for why this contract is needed: #5 handled
inline code, #16 normalized quotes, and #19 normalized Markdown delimiters;
#20 addressed paraphrased evidence, #23 extracted nested observations, #25
handled nested smart quotes, and #42 preserved grounded results across
retries. In the separate absence family, #62 made the absence schema strict,
#64 validated absence inside mixed assertions, and #66 resolved contradictions
per side. The positional validator replaces the positive-evidence extraction
chain while retaining those absence controls.
Eval authors own assertions and declared absence patterns; the judge owns
semantic judgment and source references; Shuhari owns the deterministic
validation and receipt contract.
