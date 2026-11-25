package reviewexec

import (
	"context"
	"database/sql"
	"errors"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/strictjson"
)

const (
	selectionOriginExplicit  = "explicit"
	selectionOriginAutomatic = "automatic"
)

type selectionRequestedEvent struct {
	Channel  string `json:"channel"`
	Origin   string `json:"origin,omitempty"`
	WorkerID string `json:"worker_id,omitempty"`
	sequence int64
}

func readSelectionRequestedEvent(ctx context.Context, q EvidenceQuery, r Run) (selectionRequestedEvent, error) {
	rows, err := q.QueryContext(ctx, `SELECT sequence,actor_id,
	  CASE WHEN typeof(payload_json)='text' AND length(CAST(payload_json AS BLOB))<=4096 THEN payload_json END
	  FROM events WHERE project_id=? AND event_type='review.selection_requested' AND resource_id=? AND revision=1 LIMIT 2`, r.ProjectID, r.ID)
	if err != nil {
		return selectionRequestedEvent{}, err
	}
	defer rows.Close()
	if !rows.Next() {
		return selectionRequestedEvent{}, rows.Err()
	}
	var actor any
	var sequence int64
	var body sql.NullString
	if err = rows.Scan(&sequence, &actor, &body); err != nil {
		return selectionRequestedEvent{}, err
	}
	var event selectionRequestedEvent
	if sequence < 1 || actor != r.ActorID || !body.Valid || strictjson.Decode([]byte(body.String), &event) != nil || event.Channel != r.Channel || body.String != encoded(event) {
		return selectionRequestedEvent{}, schedulingUnavailable()
	}
	switch event.Origin {
	case "":
		if event.WorkerID != "" {
			return selectionRequestedEvent{}, schedulingUnavailable()
		}
	case selectionOriginExplicit, selectionOriginAutomatic:
		if event.WorkerID != r.WorkerID {
			return selectionRequestedEvent{}, schedulingUnavailable()
		}
	default:
		return selectionRequestedEvent{}, schedulingUnavailable()
	}
	if rows.Next() {
		return selectionRequestedEvent{}, schedulingUnavailable()
	}
	event.sequence = sequence
	return event, rows.Err()
}

type automaticSelectionAttribution struct {
	revision   int64
	credential string
}

func readAutomaticSelectionAttribution(ctx context.Context, q EvidenceQuery, r Run) (automaticSelectionAttribution, error) {
	var attribution automaticSelectionAttribution
	var project, revision, actor, worker, credential any
	// Read by the unique selection ID so a wrongly scoped row cannot masquerade
	// as missing attribution for an otherwise valid selection.
	err := q.QueryRowContext(ctx, `SELECT project_id,scheduling_revision,actor_id,worker_id,credential_id
	  FROM review_automatic_selections WHERE selection_id=?`, r.ID).Scan(&project, &revision, &actor, &worker, &credential)
	if err != nil {
		return attribution, err
	}
	nativeRevision, validRevision := revision.(int64)
	credentialID, validCredential := credential.(string)
	if project != r.ProjectID || actor != r.ActorID || worker != r.WorkerID || !validRevision || nativeRevision < 1 || !validCredential || !identifier.MatchString(credentialID) {
		return attribution, schedulingUnavailable()
	}
	return automaticSelectionAttribution{revision: nativeRevision, credential: credentialID}, nil
}

func automaticSelectionBinding(ctx context.Context, tx *sql.Tx, r Run) (automaticSelectionAttribution, error) {
	event, err := readSelectionRequestedEvent(ctx, tx, r)
	if err != nil {
		return automaticSelectionAttribution{}, err
	}
	if r.ExpectedPullNumber != 0 && event.sequence == 0 {
		return automaticSelectionAttribution{}, schedulingUnavailable()
	}
	attribution, err := readAutomaticSelectionAttribution(ctx, tx, r)
	if !errors.Is(err, sql.ErrNoRows) && err != nil {
		return automaticSelectionAttribution{}, err
	}
	hasAttribution := err == nil
	switch event.Origin {
	case selectionOriginAutomatic:
		if !hasAttribution || r.ExpectedPullNumber != 0 {
			return automaticSelectionAttribution{}, schedulingUnavailable()
		}
	case selectionOriginExplicit:
		if hasAttribution {
			return automaticSelectionAttribution{}, schedulingUnavailable()
		}
	default:
		// The automatic scheduler never accepts a caller-supplied PR pin.
		if r.ExpectedPullNumber != 0 {
			if hasAttribution {
				return automaticSelectionAttribution{}, schedulingUnavailable()
			}
			return attribution, nil
		}
		if !hasAttribution {
			explicit, err := legacyExplicitSelection(ctx, tx, r)
			if err != nil {
				return automaticSelectionAttribution{}, err
			}
			if !explicit && event.sequence != 0 {
				explicit, err = selectionPredatesAutomation(ctx, tx, r, event.sequence)
				if err != nil {
					return automaticSelectionAttribution{}, err
				}
			}
			if !explicit {
				return automaticSelectionAttribution{}, schedulingUnavailable()
			}
		}
	}
	return attribution, nil
}

// Explicit accepts wrote a canonical mutation receipt in the same transaction.
// Only a positive match proves legacy explicit origin; normal current
// authorization is still independently required to admit the retained work.
func legacyExplicitSelection(ctx context.Context, q EvidenceQuery, r Run) (bool, error) {
	var hasHistory bool
	if err := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_schema WHERE type='table' AND name='restore_history')`).Scan(&hasHistory); err != nil {
		return false, err
	}
	origins := `SELECT value FROM metadata WHERE key IN ('instance_id','restore:source_instance_id')`
	if hasHistory {
		// Startup validates the connected recovery provenance of this table.
		origins += ` UNION SELECT instance_id FROM restore_history
		  UNION SELECT CASE WHEN json_valid(body_json) THEN json_extract(body_json,'$."restore:source_instance_id"') END FROM restore_history`
	}
	fingerprint, err := core.MutationRequestFingerprint(RunRequest{Channel: r.Channel}, r.WorkerID)
	if err != nil {
		return false, err
	}
	pinnedFingerprint, err := core.MutationRequestFingerprint(RunRequest{Channel: r.Channel, ExpectedConfigurationRevision: &r.ConfigurationRevision}, r.WorkerID)
	if err != nil {
		return false, err
	}
	response := encoded(Receipt{ID: r.ID, State: "queued", Revision: 1})
	var explicit bool
	err = q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM receipts
	  WHERE instance_id IN (`+origins+`) AND principal_id=? AND project_id=? AND operation='reviews.run'
	  AND target='channel-selection' AND fingerprint IN (?,?) AND response_json=?)`,
		r.ActorID, r.ProjectID, fingerprint, pinnedFingerprint, response).Scan(&explicit)
	return explicit, err
}

func selectionLocalHoldReason(ctx context.Context, q EvidenceQuery, r Run) (string, error) {
	event, err := readSelectionRequestedEvent(ctx, q, r)
	if err != nil && !automaticProjectRefused(err) {
		return "", err
	}
	var attributionPresent bool
	if err := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM review_automatic_selections WHERE selection_id=?)`, r.ID).Scan(&attributionPresent); err != nil {
		return "", err
	}
	if event.Origin == selectionOriginAutomatic || attributionPresent {
		return "automatic_scheduling_unavailable", nil
	}
	return "local_read_unavailable", nil
}

// An accepted event preceding every enabled revision of its channel is another
// positive distinction for older explicit requests with no exact receipt.
func selectionPredatesAutomation(ctx context.Context, tx *sql.Tx, r Run, acceptedSequence int64) (bool, error) {
	projected, err := readScheduling(ctx, tx, r.ProjectID)
	if err != nil || projected.Revision > 10000 {
		return false, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT sequence,revision,
	  CASE WHEN typeof(payload_json)='text' AND length(CAST(payload_json AS BLOB))<=4096 THEN payload_json END
	  FROM events WHERE project_id=? AND resource_id=? AND event_type='review.scheduling_updated'
	  ORDER BY sequence LIMIT 10001`, r.ProjectID, r.ProjectID)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	var expected int64 = 1
	for rows.Next() {
		var sequence int64
		var revision any
		var body sql.NullString
		if err := rows.Scan(&sequence, &revision, &body); err != nil {
			return false, err
		}
		var control schedulingEvent
		if revision != expected || !body.Valid || strictjson.Decode([]byte(body.String), &control) != nil || control.Hosted == nil {
			return false, nil
		}
		enabled := control.Hosted
		if r.Channel == "cli" {
			enabled = control.CLI
		}
		channelEnabled := enabled != nil && *enabled
		if sequence < acceptedSequence && channelEnabled || expected == projected.Revision && channelEnabled != automaticChannelEnabled(projected, r.Channel) {
			return false, nil
		}
		expected++
	}
	return expected == projected.Revision+1, rows.Err()
}
