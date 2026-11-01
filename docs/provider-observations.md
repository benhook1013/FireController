# Provider observation boundary

This boundary covers bounded GitHub reads, normalized history and parent facts, required-check rule decoding and pure Hosted review decoders in `internal/reviewprovider`. Callers supply repository, PR, commit, actor and command identities. Observation and parsing do not establish execution authority, native coverage, adjudication or merge readiness. The [CI contract](review-ci.md) and [execution contract](review-execution.md) own their consuming workflows.

## GitHub source reads

Observation requests retain explicit selectors, bounded response bodies, pagination and collection deadlines. Incomplete inventories, changing selectors, malformed identities and exceeded limits remain errors or unproven facts. Transport interruption differs from invalid completed JSON; safe error classes preserve provider status and retry guidance without exposing raw response bodies or credentials.

Conditional GET reuse is scoped to one authenticated adapter and exact request URL. Only a matching server `304` permits reuse of retained response bytes and pagination links. The cache is bounded; typed decoding and pagination checks still run after revalidation. A cached response never establishes collection completeness by itself.

Parent lookup returns a unique open same-repository PR whose head branch is the selected base only after a fresh matching read of that PR. A complete unpaginated empty inventory returns a zero `Pull` without an error; invalid, ambiguous or moving inventory returns an error. Queue position and historical captures cannot supply that identity. Explicit history-page readers preserve original IDs, actors, bodies, reply relationships and locations. Normalized history validation checks one selected page; cross-page completeness, durable acquisition and import authority remain caller responsibilities.

Required-check rule decoding preserves the distinction between documented rules and unavailable or unsupported authority. Decoding a requirement set does not establish current workflow success; CI selection and assessment remain with their existing owner.

## Required CI collection

CI reads bind explicit source and selected commit identities, recheck passing candidates and authority, and use exact numbered workflow-attempt jobs to distinguish same-suite reruns. Same-name commit statuses remain independent barriers rather than App identity. Count movement gets one bounded same-commit recollection; unsupported or incomplete observations remain unproven. Native retained decoding preserves exact earlier conservative origin and rerun assessments without rewriting their source facts or granting fresh credit. The [CI contract](review-ci.md) owns explicit provider-free diagnostics and the collection/retention boundary.

## Hosted review parsing

CodeRabbit decoding recognizes closed generated envelopes and presentation variants while retaining original bodies and identities. Publication attribution, exact comparison, provider run identity, finished acknowledgment and counted finding inventory remain separate checks. Inline, outside-diff, duplicate and additional findings retain their own source references. A complete single-batch, zero-inline-root publication may omit its inline count only when strict decoding proves a nonempty Additional/outside-diff inventory; omission never establishes a clean review. Quoted prompts and examples cannot supply authoritative metadata. Baseline-qualified projections distinguish narrowly supported unrelated bot edits and old finding annotations without rewriting either capture.

A finished reply alone cannot establish an empty review. Standing zero results require explicit supported inventory and coverage facts; positive submitted reviews require their own attributable submission and complete finding inventory. Allowance parsing attributes a supported reply to its original query and that query's exact operation, attempt and head, preserving rounding uncertainty in cooldown deadlines without granting execution permission.

`SubmittedReview` recognition is a parser capability in this slice; native Hosted evidence metadata and persistence have not adopted that variant here. Recognition does not grant native completion credit or establish end-to-end capture recovery. The runtime journal, engine evidence contract and backup validation own that consuming proof. Existing supported native variants retain their existing integration boundary; consumer implementation and qualification are recorded by the [execution contract](review-execution.md).

## Validation boundary

Focused validation scenarios include bounded HTTP and conditional revalidation, unique parent lookup, explicit history pages, required-check rule decoding and malformed or conflicting Hosted inventories. Qualification results belong to the slice's validation record; this document makes no installed-provider, native-recovery or operational-cutover claim.
