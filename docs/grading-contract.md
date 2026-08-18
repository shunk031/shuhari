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

Judge security selection follows the [execution security contract](architecture.md#execution-security-contract)
and is recorded in the manifest as `judge_security`. The workspace still gives
each judge only its blinded side's files. Under an acknowledged unsandboxed
resolution, that is a scoping boundary rather than kernel filesystem
isolation, so evaluators must not claim stronger protection.

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

## Grading outcome matrix

The validator evaluates each assertion result independently. The four attachment
states below describe whether the judge supplied positional evidence references,
an `absence` object, both, or neither. The `evidence` string itself is always a
required nonblank JSON field; it is not an attachment state.

The central failed-verdict rule is simple: a failed assertion needs no grounding.
A well-formed absence object may remain as auxiliary judge data, but it never
turns a judge-failed result into a validator error or a new pass/fail decision.
Malformed absence data still fails closed.

The mechanical column applies to fallback absence queries. `no-match` means the
query is absent from the artifact. `match` means the query is found and resolves
to a side-specific `contradiction` when the judge said the assertion passed.
For a judge-failed assertion, `contradiction` means that the query would match,
but the failed verdict already supplies the side-specific failure; the validator
does not search for grounding and continues with the failed result. `N/A` means
that no absence machinery is applicable.

Every fallback cell is enumerated here:

| Assertion | Judge verdict | Attachments | Mechanical state | Outcome |
| --- | --- | --- | --- | --- |
| Positive | pass | evidence refs | N/A | Valid: pass with `strong` evidence after positional verification. |
| Positive | pass | absence object | N/A | Invalid: fail closed; positive assertions cannot carry absence claims. |
| Positive | pass | both | N/A | Invalid: fail closed; positive assertions cannot carry absence claims. |
| Positive | pass | neither | N/A | Invalid: fail closed; a passing positive claim needs positional evidence. |
| Positive | fail | evidence refs | N/A | Valid: continue with a side-specific failed result; do not ground it. |
| Positive | fail | absence object | N/A | Invalid: fail closed; the absence claim is incompatible with a positive assertion. |
| Positive | fail | both | N/A | Invalid: fail closed; the absence claim is incompatible with a positive assertion. |
| Positive | fail | neither | N/A | Valid: continue with a side-specific failed result. |
| Negative | pass | evidence refs | N/A | Invalid: fail closed; a passing negative claim needs an absence declaration. |
| Negative | pass | absence object | no-match | Valid: pass with `absence` grounding; retain the query declaration. |
| Negative | pass | absence object | match | Valid: resolve as side-specific fail with `contradiction` and the matched location. |
| Negative | pass | both | no-match | Valid: pass with `absence` grounding; positional references are auxiliary. |
| Negative | pass | both | match | Valid: resolve as side-specific fail with `contradiction`; positional references are auxiliary. |
| Negative | pass | neither | N/A | Invalid: fail closed; a passing negative claim needs an absence declaration. |
| Negative | fail | evidence refs | N/A | Valid: continue with a side-specific failed result; do not ground it. |
| Negative | fail | absence object | no-match | Valid: continue with the judge-failed result; do not ground it. |
| Negative | fail | absence object | contradiction | Valid: continue with the judge-failed result; do not replace it with a new outcome. |
| Negative | fail | both | no-match | Valid: continue with the judge-failed result; do not ground it. |
| Negative | fail | both | contradiction | Valid: continue with the judge-failed result; do not ground it. |
| Negative | fail | neither | N/A | Valid: continue with a side-specific failed result. |
| Mixed | pass | evidence refs | N/A | Invalid: fail closed; a passing mixed claim needs an absence declaration too. |
| Mixed | pass | absence object | no-match | Invalid: fail closed; the positive clause also needs positional evidence. |
| Mixed | pass | absence object | match | Invalid: fail closed; the positive clause also needs positional evidence. |
| Mixed | pass | both | no-match | Valid: pass with `absence` grounding and exact positional evidence for the positive clause. |
| Mixed | pass | both | match | Valid: resolve as side-specific fail with `contradiction` after verifying the positive clause. |
| Mixed | pass | neither | N/A | Invalid: fail closed; both positive evidence and absence data are required. |
| Mixed | fail | evidence refs | N/A | Valid: continue with a side-specific failed result; do not ground it. |
| Mixed | fail | absence object | no-match | Valid: continue with the judge-failed result; do not ground it. |
| Mixed | fail | absence object | contradiction | Valid: continue with the judge-failed result; do not ground it. |
| Mixed | fail | both | no-match | Valid: continue with the judge-failed result; do not ground it. |
| Mixed | fail | both | contradiction | Valid: continue with the judge-failed result; do not ground it. |
| Mixed | fail | neither | N/A | Valid: continue with a side-specific failed result. |

For a declared `forbidden_patterns` negative assertion, the eval-owned patterns
replace the judge's absence query. The four attachment states are still valid
inputs because the patterns are authoritative; a supplied fallback absence
object must nevertheless be structurally valid. The complete declared-pattern
matrix is:

| Judge verdict | Attachments | no-match | match / contradiction |
| --- | --- | --- | --- |
| pass | evidence refs | Valid: pass as `absence`; record the declared patterns. | Valid: side-specific fail as `contradiction`; record the matched location. |
| pass | absence object | Valid: pass as `absence`; declared patterns take precedence. | Valid: side-specific fail as `contradiction`; declared patterns take precedence. |
| pass | both | Valid: pass as `absence`; declared patterns take precedence. | Valid: side-specific fail as `contradiction`; declared patterns take precedence. |
| pass | neither | Valid: pass as `absence`; declared patterns are sufficient. | Valid: side-specific fail as `contradiction`; declared patterns are sufficient. |
| fail | evidence refs | Valid: continue with the judge-failed result; do not ground it. | Valid: continue with the judge-failed result; do not ground it. |
| fail | absence object | Valid: continue with the judge-failed result; do not ground it. | Valid: continue with the judge-failed result; do not ground it. |
| fail | both | Valid: continue with the judge-failed result; do not ground it. | Valid: continue with the judge-failed result; do not ground it. |
| fail | neither | Valid: continue with the judge-failed result; do not ground it. | Valid: continue with the judge-failed result; do not ground it. |

An absence object with a blank field, a clause that is
not a verbatim assertion substring, or an assertion type that cannot be
negative/mixed is malformed and fails closed in every verdict state. Missing,
duplicate, mismatched, or extra grader cases/assertions also fail closed.

The table is exercised by the generated 48-cell table-driven corpus in
`internal/eval/grading_contract_matrix_test.go`: 32 fallback cells and 16
declared-pattern cells. Its retained regression rows cover: iteration-27's
mixed absence clause omission; iteration-28's hallucinated negative absence
against an artifact contradiction; iteration-30's malformed declared-absence
object; iteration-31's generic non-verbatim positive evidence; and iteration-34's
well-formed failed mixed result carrying absence data.

## Comparator path audit

Comparators remain prompt-rendered. They compare two outputs and have a different
failure surface from positive evidence grounding: their contract is a blind A/B
preference and a nonblank reason, not a claim that a quoted excerpt came from one
artifact. The comparator still receives the existing blinded A/B artifacts and
original task fields, and keeps the existing validation, retry, and watchdog
behavior. Converting comparators to agents is out of scope for #13.

The audit found no well-formed comparator cell that aborts grading, so no
comparator production change is required. The table below records the malformed
cells that still retry and fail closed.

| Comparator cell | Outcome |
| --- | --- |
| Exactly one requested case with preferred `A`, `B`, or `tie` and a nonblank reason | Valid: resolve the preference and continue. |
| `tie`, including when neither output is materially better | Valid: record a tie and continue. |
| Nonblank but weak or generic reason | Valid: the comparator contract requires nonblank text, not a quality threshold. |
| Invalid preferred value | Invalid: retry once, then fail closed if still invalid. |
| Blank reason | Invalid: retry once, then fail closed if still blank. |
| Missing, extra, duplicate, or mismatched case/trial | Invalid: retry once, then fail closed if still structurally incomplete. |
| Transport failure before completion | Retry under the normal harness transport limit; fail closed after exhaustion. |
| Completed response with a well-formed preference | Never retry as transport; resolve the preference. |

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
