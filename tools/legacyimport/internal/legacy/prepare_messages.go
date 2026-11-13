package legacy

import (
	"context"
	"database/sql"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/benhook1013/FireController/internal/coordimport"
	"github.com/benhook1013/FireController/internal/coordimportmodel"
	"github.com/benhook1013/FireController/internal/inbox"
)

type MessageSelection struct {
	Version               int               `json:"version"`
	RepositoryID          int64             `json:"repository_id"`
	ExpectedRevision      *int64            `json:"expected_revision"`
	Source                string            `json:"source"`
	MessageIDs            []string          `json:"message_ids"`
	Workers               map[string]string `json:"workers"`
	UnknownAuthorWorkerID string            `json:"unknown_author_worker_id,omitempty"`
	RetainedParentIDs     []string          `json:"retained_parent_ids,omitempty"`
	AssociationPolicy     string            `json:"association_policy"`
}
type MessagePreparationSummary struct {
	Kind                 string `json:"kind"`
	SourceSHA256         string `json:"source_sha256"`
	Messages             int    `json:"messages"`
	Replies              int    `json:"replies"`
	ArchivedAssociations int    `json:"archived_associations"`
	UnknownAuthors       int    `json:"unknown_authors"`
}

func messageConversionBlocked() error {
	return &Error{Code: "snapshot_conversion_blocked", Message: "The selected messages or dependencies cannot be represented without changing source facts; inspect explicit worker, parent and archive selections."}
}
func messageSourceKey(s string) bool {
	return s != "" && len(s) <= 200 && utf8.ValidString(s) && strings.TrimSpace(s) != ""
}
func messageNativeWorker(s string) bool {
	if len(s) < 1 || len(s) > 128 {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

type selectedMessage struct {
	item         coordimportmodel.PreparedMessage
	at           time.Time
	associations int
	unknown      bool
}

func readSelectedMessage(ctx context.Context, db *sql.DB, id string, selection MessageSelection) (selectedMessage, error) {
	var out selectedMessage
	rows, err := db.QueryContext(ctx, `SELECT id,recipient,body,author,job,pr,reply_to,created_at,seen_at,acknowledged_at,typeof(id),typeof(recipient),typeof(body),typeof(author),typeof(job),typeof(pr),typeof(reply_to),typeof(created_at),typeof(seen_at),typeof(acknowledged_at) FROM inbox_messages WHERE id=? LIMIT 2`, id)
	if err != nil {
		return out, messageConversionBlocked()
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		count++
		var sourceID, recipient, body, created string
		var author, job, reply, seen, ack sql.NullString
		var pr sql.NullInt64
		var kinds [10]string
		if err = rows.Scan(&sourceID, &recipient, &body, &author, &job, &pr, &reply, &created, &seen, &ack, &kinds[0], &kinds[1], &kinds[2], &kinds[3], &kinds[4], &kinds[5], &kinds[6], &kinds[7], &kinds[8], &kinds[9]); err != nil {
			return out, messageConversionBlocked()
		}
		if count != 1 || sourceID != id || kinds[0] != "text" || kinds[1] != "text" || kinds[2] != "text" || kinds[7] != "text" {
			return out, messageConversionBlocked()
		}
		for _, i := range []int{3, 4, 6, 8, 9} {
			if kinds[i] != "text" && kinds[i] != "null" {
				return out, messageConversionBlocked()
			}
		}
		if kinds[5] != "integer" && kinds[5] != "null" {
			return out, messageConversionBlocked()
		}
		if !messageSourceKey(recipient) || author.Valid && !messageSourceKey(author.String) || job.Valid && !messageSourceKey(job.String) || pr.Valid && pr.Int64 <= 0 {
			return out, messageConversionBlocked()
		}
		item := coordimportmodel.PreparedMessage{SourceKey: sourceID, ReplyToSourceKey: reply.String, Body: body, CreatedAt: created, SeenAt: seen.String, AcknowledgedAt: ack.String}
		var ok bool
		item.RecipientWorkerID, ok = selection.Workers[recipient]
		if !ok {
			return out, messageConversionBlocked()
		}
		if author.Valid {
			item.SenderWorkerID, ok = selection.Workers[author.String]
			if !ok {
				return out, messageConversionBlocked()
			}
		} else {
			item.SenderWorkerID = selection.UnknownAuthorWorkerID
			out.unknown = true
		}
		// Only actual NULL handling facts map to absent native optional fields.
		if reply.Valid && reply.String == "" || seen.Valid && seen.String == "" || ack.Valid && ack.String == "" {
			return out, messageConversionBlocked()
		}
		if _, err = inbox.NormalizePreparedMessage(item); err != nil {
			return out, messageConversionBlocked()
		}
		out.item = item
		out.at, err = time.Parse(time.RFC3339Nano, created)
		if err != nil {
			return out, messageConversionBlocked()
		}
		if job.Valid {
			out.associations++
		}
		if pr.Valid {
			out.associations++
		}
	}
	if rows.Err() != nil || count != 1 {
		return out, messageConversionBlocked()
	}
	return out, nil
}

// PrepareMessages reads only explicitly selected IDs. Retained parents are
// validated source facts, not added output; their unselected ancestry belongs
// to the destination's authoritative retained mapping/graph check.
func PrepareMessages(ctx context.Context, path string, limits Limits, selection MessageSelection) (coordimportmodel.Batch, MessagePreparationSummary, error) {
	blocked := func() (coordimportmodel.Batch, MessagePreparationSummary, error) {
		return coordimportmodel.Batch{}, MessagePreparationSummary{}, messageConversionBlocked()
	}
	if selection.Version != 1 || selection.RepositoryID <= 0 || selection.ExpectedRevision == nil || *selection.ExpectedRevision < 0 || len(selection.MessageIDs) < 1 || len(selection.MessageIDs) > 100 || len(selection.RetainedParentIDs) > 100 || len(selection.Workers) > 1000 || selection.AssociationPolicy != "archive" {
		return blocked()
	}
	if selection.UnknownAuthorWorkerID != "" && !messageNativeWorker(selection.UnknownAuthorWorkerID) {
		return blocked()
	}
	for k, v := range selection.Workers {
		if !messageSourceKey(k) || !messageNativeWorker(v) {
			return blocked()
		}
	}
	selected, retained := map[string]bool{}, map[string]bool{}
	for _, id := range selection.MessageIDs {
		if !messageSourceKey(id) || selected[id] {
			return blocked()
		}
		selected[id] = true
	}
	for _, id := range selection.RetainedParentIDs {
		if !messageSourceKey(id) || selected[id] || retained[id] {
			return blocked()
		}
		retained[id] = true
	}
	var batch coordimportmodel.Batch
	summary := MessagePreparationSummary{Kind: "prepared_message_import"}
	_, err := withInspection(ctx, path, limits, func(db *sql.DB, inspection Inspection) error {
		if inspection.Versions.Inbox == 0 {
			return messageConversionBlocked()
		}
		nodes := make(map[string]selectedMessage, len(selected)+len(retained))
		for _, ids := range [][]string{selection.MessageIDs, selection.RetainedParentIDs} {
			for _, id := range ids {
				v, e := readSelectedMessage(ctx, db, id, selection)
				if e != nil {
					return e
				}
				nodes[id] = v
			}
		}
		for _, id := range selection.MessageIDs {
			v := nodes[id]
			if p := v.item.ReplyToSourceKey; p != "" {
				parent, ok := nodes[p]
				if !ok {
					return messageConversionBlocked()
				}
				a, b := v.item, parent.item
				if !((a.SenderWorkerID == b.SenderWorkerID && a.RecipientWorkerID == b.RecipientWorkerID) || (a.SenderWorkerID == b.RecipientWorkerID && a.RecipientWorkerID == b.SenderWorkerID)) {
					return messageConversionBlocked()
				}
			}
		}
		// Detect every cycle visible in the bounded selected+explicit-parent graph.
		// Do not read missing ancestors of an explicitly retained parent.
		state := map[string]uint8{}
		var visit func(string) error
		visit = func(id string) error {
			if state[id] == 1 {
				return messageConversionBlocked()
			}
			if state[id] == 2 {
				return nil
			}
			state[id] = 1
			p := nodes[id].item.ReplyToSourceKey
			if _, ok := nodes[p]; p != "" && ok {
				if err := visit(p); err != nil {
					return err
				}
			}
			state[id] = 2
			return nil
		}
		for id := range nodes {
			if e := visit(id); e != nil {
				return e
			}
		}
		chosen := make([]selectedMessage, 0, len(selected))
		for _, id := range selection.MessageIDs {
			chosen = append(chosen, nodes[id])
		}
		sort.Slice(chosen, func(i, j int) bool {
			if chosen[i].at.Equal(chosen[j].at) {
				return chosen[i].item.SourceKey < chosen[j].item.SourceKey
			}
			return chosen[i].at.Before(chosen[j].at)
		})
		batch = coordimportmodel.Batch{Version: 1, RepositoryID: selection.RepositoryID, ExpectedRevision: selection.ExpectedRevision, Source: selection.Source, SourceSHA256: inspection.SHA256, Jobs: []coordimportmodel.PreparedJob{}, Messages: make([]coordimportmodel.PreparedMessage, 0, len(chosen))}
		for _, v := range chosen {
			batch.Messages = append(batch.Messages, v.item)
			summary.Messages++
			summary.ArchivedAssociations += v.associations
			if v.unknown {
				summary.UnknownAuthors++
			}
			if v.item.ReplyToSourceKey != "" {
				summary.Replies++
			}
		}
		normalized, _, e := coordimport.Normalize(batch)
		if e != nil {
			return messageConversionBlocked()
		}
		batch = normalized
		summary.SourceSHA256 = inspection.SHA256
		return nil
	})
	if err != nil {
		return coordimportmodel.Batch{}, MessagePreparationSummary{}, err
	}
	return batch, summary, nil
}
