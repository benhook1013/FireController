# Copyright 2026 FireController contributors. SPDX-License-Identifier: Apache-2.0
"""Source-side composition of canonical audits and policy projections.

The caller must supply explicit adapters within a paused source acquisition
procedure and verify its database/capture/Git provenance and conservation.
This component neither acquires that pause nor posts review decisions.
"""
import json


class FrameRefusal(Exception):
    pass


def acquire_frame(controller, records, policy, *, repository_id, snapshot_sha256,
                  selected, writer_pause_confirmed, history_validator):
    if writer_pause_confirmed is not True:
        raise FrameRefusal("source_writers_not_paused")
    if (type(repository_id) is not int or not 0 < repository_id < 2**63
            or not isinstance(snapshot_sha256, str) or len(snapshot_sha256) != 64
            or any(c not in '0123456789abcdef' for c in snapshot_sha256)):
        raise FrameRefusal("source_binding_invalid")
    if (type(selected) is not dict or not 1 <= len(selected) <= 100
            or any(type(pr) is not int or not 0 < pr < 2**31
                   or type(channels) is not tuple or not channels
                   or len(channels) != len(set(channels))
                   or any(channel not in ('cli', 'hosted') for channel in channels)
                   for pr, channels in selected.items())):
        raise FrameRefusal("source_selection_invalid")
    if controller.isolated_fixture:
        raise FrameRefusal("fixture_clearance_forbidden")
    state = controller.store.load()
    if not set(selected) <= set(state.ordered_prs):
        raise FrameRefusal("source_selection_not_configured")
    evidence = controller._evidence_provider

    def idle():
        if evidence.repository_cli_lock_status() != 'clear':
            raise FrameRefusal("source_cli_lock_uncertain")
        # Current-request readers deliberately exclude completed review history.
        # Requiring empty results is conservative: even a terminal diagnostic
        # must be understood rather than silently classified as settled here.
        for pr in state.ordered_prs:
            for channel in ('cli', 'hosted'):
                rows = evidence.request_history(pr, channel)
                if type(rows) not in (list, tuple) or rows:
                    raise FrameRefusal("source_requests_not_clear")

    def routes_clear(pr):
        for direction in ('target_pr', 'source_pr'):
            rows = records.list_routes(status='open', include_legacy_routes=True,
                                       **{direction: pr})
            if type(rows) is not list or rows:
                raise FrameRefusal("source_routes_not_clear")

    idle()
    before_state = state.to_dict()
    cache, audit_cache = {}, {}
    live, reconciliation = controller._reconciliation(
        state, evidence_prs=set(selected), history_cache=cache,
        prefetch_complete_history=True)
    pulls = []
    for pr, channels in selected.items():
        routes_clear(pr)
        current = controller._reconciled_anchor(pr, live[pr], reconciliation)
        if current is None:
            raise FrameRefusal("source_anchor_unavailable")
        transfer_audit = getattr(controller, '_check_transfer_evidence', None)
        if transfer_audit is not None and not callable(transfer_audit):
            raise FrameRefusal("source_transfer_audit_unsupported")
        for channel in channels:
            arguments = dict(stop_audit_cache=audit_cache, history_cache=cache)
            if transfer_audit is None:
                # Older trusted helpers retain their positive-checkpoint contract.
                controller._check_stop_evidence(
                    state, pr, policy.Channel(channel), current, reconciliation,
                    checkpoint_pin=None, **arguments)
            else:
                transfer_audit(state, pr, policy.Channel(channel), current,
                               reconciliation, **arguments)
        histories = {channel: controller._policy_history(
            state, pr, channel, reconciliation, history_cache=cache)
            for channel in (policy.Channel.CLI, policy.Channel.HOSTED)}
        histories[policy.Channel.CLI] = controller._project_cli_streak_lineage(
            controller._project_cli_hosted_reservations(
                histories[policy.Channel.CLI], histories[policy.Channel.HOSTED],
                pr, live[pr].head, current,
                controller._actual_hosted_overlap_anchor(
                    live[pr], histories[policy.Channel.HOSTED])),
            pr, state, current)
        pulls.append({'pull_number': pr, 'channels': {
            channel: histories[policy.Channel(channel)] for channel in channels}})
        routes_clear(pr)
    idle()
    if controller.store.load().to_dict() != before_state:
        raise FrameRefusal("source_state_changed")
    frame = dict(version=1, repository_id=repository_id,
                 snapshot_sha256=snapshot_sha256, projection_complete=True,
                 quiescent=True, obligations_clear=True, prs=pulls)
    # Fail rather than truncate a frame or silently coerce source row types.
    try:
        body = json.dumps(frame, allow_nan=False, ensure_ascii=True,
                          sort_keys=True, separators=(',', ':')).encode()
    except (TypeError, ValueError, RecursionError) as error:
        raise FrameRefusal('source_frame_not_serializable') from error
    if len(body)+1 > 1 << 20:
        raise FrameRefusal('source_frame_too_large')
    # A source-valid legacy fix may still be unsupported by the chosen native
    # transfer boundary. Reuse that boundary's validator; never invent a
    # structured source-resolution status from a corrected-head inference.
    for pr, channels in selected.items():
        for channel in channels:
            history_validator(frame, {'repository_id': repository_id,
                                     'pull_number': pr, 'channel': channel},
                              snapshot_sha256)
    return frame
