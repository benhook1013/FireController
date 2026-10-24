# Copyright 2026 FireController contributors. SPDX-License-Identifier: Apache-2.0
"""Narrow, explicit source audit extension for retained terminal results.

The source's complete audit already accepts an exact known held-observation
fingerprint. Derive that identity only after positive archived/current result
proof. Preserve the held row and all counting flags; its controls remain
deferred. Ordinary source evidence classes and stop behavior are unchanged.
"""
from types import SimpleNamespace

from source_carried_frame import canonical, row_digest
from source_obligation_results import validate_unsettled_hosted
from source_obligation_routes import _pr, _source_key


class CarriedEvidenceRefusal(Exception):
    def __init__(self):
        super().__init__('carried_evidence_unverified')


def evidence_class(helpers, result_runs):
    """Build a dedicated collector class for an explicit sealed run selection.

    The caller owns immutable records, complete repository-bound collection,
    capture/Git confinement, writer pause and conservation. This is not a
    numeric-transfer or old stop authorization API.
    """
    if (type(result_runs) is not dict or not 1 <= len(result_runs) <= 100
            or any(not _pr(pr) or type(runs) is not tuple or not 1 <= len(runs) <= 100
                   or any(not _source_key(run) or len(run.encode()) > 100 for run in runs)
                   or len(set(runs)) != len(runs) for pr, runs in result_runs.items())):
        raise CarriedEvidenceRefusal()
    selected = {pr: tuple(runs) for pr, runs in result_runs.items()}
    base = helpers['runtime'].LiveEvidence
    fingerprint = helpers['state'].observation_fingerprint

    class CarriedEvidence(base):
        def __init__(self, *args, **kwargs):
            super().__init__(*args, **kwargs)
            self.carried_result_proofs = {}

        def _audit_complete_hosted_history(self, pr, expected_hosted_fingerprints,
                                          expected_anchor, *, payload,
                                          allow_historical_unmatched=False, now=None):
            proofs, retained, seen = {}, list(expected_hosted_fingerprints), set()
            before = row_digest(payload)
            if pr in selected:
                history = self.records.history(pr, include_legacy_routes=True, include_display=False)
                for run in selected[pr]:
                    result = validate_unsettled_hosted(
                        helpers, self.records, history, run, self.repo, payload)
                    proof = result['proof']
                    observation = base._uncheckpointed_hosted_observation(pr,
                        SimpleNamespace(head_sha=proof['head_sha'], response_id=proof['response_id']))
                    # An exact canonical held row must still be visible. The
                    # audit recognition supplies no checkpoint or completion.
                    rows = self.history(pr, 'hosted', now=now)
                    if sum(canonical(row) == canonical(observation) for row in rows) != 1:
                        raise CarriedEvidenceRefusal()
                    identity = fingerprint(observation)
                    if identity in seen:
                        raise CarriedEvidenceRefusal()
                    seen.add(identity)
                    retained.append(identity)
                    proofs[(pr, run)] = dict(result=result,
                        observation_sha256=row_digest(observation),
                        observation_fingerprint=identity, audited_payload_sha256=before)
            audit = super()._audit_complete_hosted_history(pr, tuple(retained), expected_anchor,
                payload=payload, allow_historical_unmatched=allow_historical_unmatched, now=now)
            if row_digest(payload) != before:
                raise CarriedEvidenceRefusal()
            self.carried_result_proofs.update(proofs)
            return audit

    return CarriedEvidence
