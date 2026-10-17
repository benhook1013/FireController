package legacy

import (
	"context"
	"database/sql"
	"encoding/json"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/benhook1013/FireController/internal/strictjson"
)

func diagnostic(result *Inspection, code string, count int64) {
	if count > 0 {
		result.Diagnostics = append(result.Diagnostics, Diagnostic{code, count})
	}
}
func inspectJobs(ctx context.Context, db *sql.DB, result *Inspection) error {
	rows, err := db.QueryContext(ctx, `SELECT typeof(brief),brief FROM job_briefs`)
	if err != nil {
		return safeSQL(ctx, err)
	}
	for rows.Next() {
		var kind string
		var body sql.NullString
		if err = rows.Scan(&kind, &body); err != nil {
			break
		}
		result.Briefs.Count++
		size := int64(len(body.String))
		if size > result.Briefs.MaxUTF8Bytes {
			result.Briefs.MaxUTF8Bytes = size
		}
		if size > result.Briefs.NativeLimitBytes {
			result.Briefs.OverNativeLimit++
		}
		if kind != "text" || !body.Valid || !utf8.ValidString(body.String) {
			result.Briefs.InvalidText++
		}
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return safeSQL(ctx, err)
	}
	rows, err = db.QueryContext(ctx, `SELECT kind FROM job_notes`)
	if err != nil {
		return safeSQL(ctx, err)
	}
	for rows.Next() {
		var kind sql.NullString
		if err = rows.Scan(&kind); err != nil {
			break
		}
		if kind.String == "instruction" {
			result.Notes.InstructionNotes++
		}
		if !kind.Valid || kind.String != "reminder" && kind.String != "source" {
			result.Notes.UnsupportedKinds++
		}
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return safeSQL(ctx, err)
	}
	rows, err = db.QueryContext(ctx, `SELECT state_json FROM job_note_revisions`)
	if err != nil {
		return safeSQL(ctx, err)
	}
	for rows.Next() {
		var body sql.NullString
		if err = rows.Scan(&body); err != nil {
			break
		}
		var state map[string]json.RawMessage
		var kind string
		if !body.Valid || strictjson.Validate([]byte(body.String)) != nil || json.Unmarshal([]byte(body.String), &state) != nil || state == nil || json.Unmarshal(state["kind"], &kind) != nil || string(state["kind"]) == "null" {
			result.Notes.InvalidRevisions++
			continue
		}
		if kind == "instruction" {
			result.Notes.InstructionRevisions++
		}
		if kind != "reminder" && kind != "source" {
			result.Notes.UnsupportedRevisionKinds++
		}
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return safeSQL(ctx, err)
	}
	diagnostic(result, "brief_over_native_limit", result.Briefs.OverNativeLimit)
	diagnostic(result, "brief_invalid_text", result.Briefs.InvalidText)
	diagnostic(result, "unsupported_note_kind", result.Notes.UnsupportedKinds)
	diagnostic(result, "unsupported_note_revision_kind", result.Notes.UnsupportedRevisionKinds)
	diagnostic(result, "invalid_note_revision", result.Notes.InvalidRevisions)
	return nil
}
func validLegacyText(value sql.NullString, required bool, maximum int) bool {
	return value.Valid && utf8.ValidString(value.String) && (!required || strings.TrimSpace(value.String) != "") && utf8.RuneCountInString(value.String) <= maximum
}
func inspectMessages(ctx context.Context, db *sql.DB, result *Inspection) error {
	result.Messages.HandlingTimestampsAvailable = true
	rows, err := db.QueryContext(ctx, `SELECT id,recipient,body,author,job,pr,reply_to,created_at,seen_at,acknowledged_at FROM inbox_messages`)
	if err != nil {
		return safeSQL(ctx, err)
	}
	type link struct {
		parent    string
		hasParent bool
	}
	parents := map[string]link{}
	for rows.Next() {
		var id, recipient, body, author, job, reply, created, seen, ack sql.NullString
		var pr sql.NullInt64
		if err = rows.Scan(&id, &recipient, &body, &author, &job, &pr, &reply, &created, &seen, &ack); err != nil {
			break
		}
		for _, check := range []struct {
			value    sql.NullString
			required bool
			maximum  int
		}{{id, true, 100}, {recipient, true, 100}, {body, true, 20000}, {created, true, 100}} {
			if !validLegacyText(check.value, check.required, check.maximum) {
				result.Messages.InvalidFields++
			}
		}
		for _, value := range []sql.NullString{author, job, reply} {
			if value.Valid && !validLegacyText(value, true, 100) {
				result.Messages.InvalidFields++
			}
		}
		if pr.Valid && pr.Int64 <= 0 {
			result.Messages.InvalidFields++
		}
		if !author.Valid || strings.TrimSpace(author.String) == "" {
			result.Messages.UnknownAuthors++
		}
		if reply.Valid {
			result.Messages.Replies++
			if reply.String == id.String {
				result.Messages.SelfReplies++
			}
		}
		if seen.Valid {
			result.Messages.Seen++
		}
		if ack.Valid {
			result.Messages.Acknowledged++
		}
		if ack.Valid && !seen.Valid {
			result.Messages.AckWithoutSeen++
		}
		for _, value := range []sql.NullString{created, seen, ack} {
			if value.Valid {
				if _, e := time.Parse(time.RFC3339Nano, value.String); e != nil {
					result.Messages.InvalidTimestamps++
				}
			}
		}
		if id.Valid {
			if _, duplicate := parents[id.String]; duplicate {
				result.Messages.InvalidFields++
			} else if !reply.Valid {
				parents[id.String] = link{}
			} else {
				parents[id.String] = link{reply.String, true}
			}
		}
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return safeSQL(ctx, err)
	}
	for _, parent := range parents {
		if parent.hasParent {
			if _, ok := parents[parent.parent]; !ok {
				result.Messages.OrphanReplies++
			}
		}
	}
	// Functional-graph walks count actual cycle members, not every descendant
	// whose ancestry reaches one. Each bounded source identity is visited once.
	done := map[string]bool{}
	for id := range parents {
		path := []string{}
		positions := map[string]int{}
		current := id
		for !done[current] {
			if start, ok := positions[current]; ok {
				result.Messages.CyclicReplyMessages += int64(len(path) - start)
				break
			}
			parent, ok := parents[current]
			if !ok {
				break
			}
			positions[current] = len(path)
			path = append(path, current)
			if !parent.hasParent {
				break
			}
			current = parent.parent
		}
		for _, seen := range path {
			done[seen] = true
		}
	}
	diagnostic(result, "message_orphan_reply", result.Messages.OrphanReplies)
	diagnostic(result, "message_self_reply", result.Messages.SelfReplies)
	diagnostic(result, "message_reply_cycle", result.Messages.CyclicReplyMessages)
	diagnostic(result, "message_ack_without_seen", result.Messages.AckWithoutSeen)
	diagnostic(result, "message_invalid_fields", result.Messages.InvalidFields)
	diagnostic(result, "message_invalid_timestamp", result.Messages.InvalidTimestamps)
	return nil
}
func inspectControllerState(ctx context.Context, db *sql.DB, result *Inspection) error {
	var body sql.NullString
	if err := db.QueryRowContext(ctx, `SELECT state_json FROM review_state WHERE singleton=1`).Scan(&body); err != nil {
		return safeSQL(ctx, err)
	}
	var state map[string]json.RawMessage
	var version int64
	if !body.Valid || strictjson.Validate([]byte(body.String)) != nil || json.Unmarshal([]byte(body.String), &state) != nil || state == nil || json.Unmarshal(state["schema_version"], &version) != nil || version != result.Versions.Controller {
		diagnostic(result, "controller_state_invalid", 1)
	}
	sort.Slice(result.Diagnostics, func(i, j int) bool { return result.Diagnostics[i].Code < result.Diagnostics[j].Code })
	return nil
}
