package projectmap

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/strictjson"
)

func readImportMapping(ctx context.Context, tx *sql.Tx, project, source, key string) (ImportMapping, bool, error) {
	m := ImportMapping{SourceKey: key}
	err := tx.QueryRowContext(ctx, `SELECT entry_id,entry_revision,source_revision FROM project_map_import_items WHERE project_id=? AND source_key=? AND item_key=?`, project, source, key).Scan(&m.EntryID, &m.EntryRevision, &m.SourceRevision)
	if errors.Is(err, sql.ErrNoRows) {
		return m, false, nil
	}
	if err != nil {
		return m, false, err
	}
	if !entryID.MatchString(m.EntryID) || m.EntryRevision < 1 || m.SourceRevision < 1 {
		return m, false, unavailable()
	}
	return m, true, nil
}
func decodeImportRecord(raw string) (ImportRecord, error) {
	var record ImportRecord
	if len(raw) > MaxImportRecordBytes || strictjson.Decode([]byte(raw), &record) != nil || record.Revision < 1 || !importKey.MatchString(record.SourceKey) || record.SourceKey != record.Document.Source.Key || !validID(record.ActorID) || !validID(record.WorkerID) || !validStamp(record.CreatedAt) {
		return record, unavailable()
	}
	normalized, err := NormalizeImportDocument(record.Document)
	if err != nil || !sameImportDocument(normalized, record.Document) || len(record.Mappings) != len(record.Document.Entries) {
		return record, unavailable()
	}
	seen := map[string]bool{}
	for n, m := range record.Mappings {
		if m.SourceKey != record.Document.Entries[n].SourceKey || !entryID.MatchString(m.EntryID) || seen[m.EntryID] || m.EntryRevision < 1 || m.SourceRevision != record.Revision {
			return record, unavailable()
		}
		seen[m.EntryID] = true
	}
	canonical, err := json.Marshal(record)
	if err != nil || string(canonical) != raw {
		return record, unavailable()
	}
	return record, nil
}
func readImportRecord(ctx context.Context, tx *sql.Tx, project, source string, revision int64) (ImportRecord, error) {
	var raw sql.NullString
	var rowRevision int64
	var actor, worker, at string
	var err error
	if revision == 0 {
		// Scalar pointer and latest immutable source record must agree even on
		// ordinary reads. This stays bounded to one record, not its whole history.
		err = tx.QueryRowContext(ctx, `SELECT s.revision,CASE WHEN length(CAST(s.record_json AS BLOB))<=? AND s.record_json=h.record_json THEN s.record_json ELSE NULL END,h.actor_id,h.worker_id,h.created_at FROM project_map_import_sources s JOIN project_map_import_history h ON h.project_id=s.project_id AND h.source_key=s.source_key AND h.revision=s.revision WHERE s.project_id=? AND s.source_key=?`, MaxImportRecordBytes, project, source).Scan(&rowRevision, &raw, &actor, &worker, &at)
	} else {
		err = tx.QueryRowContext(ctx, `SELECT revision,CASE WHEN length(CAST(record_json AS BLOB))<=? THEN record_json ELSE NULL END,actor_id,worker_id,created_at FROM project_map_import_history WHERE project_id=? AND source_key=? AND revision=?`, MaxImportRecordBytes, project, source, revision).Scan(&rowRevision, &raw, &actor, &worker, &at)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return ImportRecord{}, notFound()
	}
	if err != nil {
		return ImportRecord{}, err
	}
	if !raw.Valid {
		return ImportRecord{}, unavailable()
	}
	record, err := decodeImportRecord(raw.String)
	if err != nil {
		return ImportRecord{}, err
	}
	if record.SourceKey != source || record.Revision != rowRevision || record.ActorID != actor || record.WorkerID != worker || record.CreatedAt != at {
		return ImportRecord{}, unavailable()
	}
	return record, nil
}
func existingImportRecord(ctx context.Context, tx *sql.Tx, project, source string) (ImportRecord, bool, error) {
	var exists int
	err := tx.QueryRowContext(ctx, `SELECT 1 FROM project_map_import_sources WHERE project_id=? AND source_key=?`, project, source).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return ImportRecord{}, false, nil
	}
	if err != nil {
		return ImportRecord{}, false, err
	}
	record, err := readImportRecord(ctx, tx, project, source, 0)
	return record, true, err
}
func planImport(ctx context.Context, tx *sql.Tx, project string, doc ImportDocument) ([]ImportPlanItem, int, error) {
	var repository int64
	if err := tx.QueryRowContext(ctx, `SELECT github_repository_id FROM projects WHERE id=?`, project).Scan(&repository); err != nil {
		return nil, 0, err
	}
	if repository != doc.Source.RepositoryID {
		return nil, 0, invalid()
	}
	retained, retainedExists, err := existingImportRecord(ctx, tx, project, doc.Source.Key)
	if err != nil {
		return nil, 0, err
	}
	retainedTargetsMatch := retainedExists && sameImportDocument(retained.Document, doc)
	plan := make([]ImportPlanItem, 0, len(doc.Entries))
	conflicts := 0
	for n, item := range doc.Entries {
		m, exists, err := readImportMapping(ctx, tx, project, doc.Source.Key, item.SourceKey)
		if err != nil {
			return nil, 0, err
		}
		p := ImportPlanItem{SourceKey: item.SourceKey, Action: "create", Reason: "new_source_item"}
		if !exists {
			retainedTargetsMatch = false
			if item.ExpectedRevision != 0 {
				p.Action = "conflict"
				p.Reason = "unmapped_source_item"
			}
		} else {
			current, err := readEntry(ctx, tx, project, m.EntryID)
			if err != nil {
				return nil, 0, err
			}
			p.EntryID = current.ID
			p.CurrentRevision = current.Revision
			// The exception is bound to the complete latest approved document
			// and every captured target revision. Equal text after a manual
			// edit (including an edit-back) does not restore that authority.
			if retainedTargetsMatch && (m != retained.Mappings[n] || current.Revision != m.EntryRevision || !equalEditable(editable(current), item.Entry)) {
				retainedTargetsMatch = false
			}
			switch {
			case item.ExpectedRevision == 0 && current.Revision != m.EntryRevision:
				p.Action = "conflict"
				p.Reason = "entry_changed"
			case item.ExpectedRevision == 0 && !equalEditable(editable(current), item.Entry):
				p.Action = "conflict"
				p.Reason = "source_content_changed"
			case item.ExpectedRevision > 0 && item.ExpectedRevision != current.Revision:
				p.Action = "conflict"
				p.Reason = "revision_changed"
			default:
				changed, err := admitEntryReplacement(current, current.Revision, item.Entry)
				if err != nil {
					p.Action = "conflict"
					p.Reason = "map_contract_conflict"
				} else if changed {
					p.Action = "update"
					p.Reason = "approved_update"
				} else {
					p.Action = "unchanged"
					p.Reason = "content_unchanged"
				}
			}
		}
		if p.Action == "conflict" {
			conflicts++
		}
		plan = append(plan, p)
	}
	if retainedTargetsMatch {
		for n := range plan {
			if plan[n].Action == "conflict" && plan[n].Reason == "revision_changed" && doc.Entries[n].ExpectedRevision > 0 && plan[n].CurrentRevision > doc.Entries[n].ExpectedRevision {
				plan[n].Action = "unchanged"
				plan[n].Reason = "retained_document_unchanged"
				conflicts--
			}
		}
	}
	return plan, conflicts, nil
}
func applyPreparedImport(ctx context.Context, store *core.Store, scope core.Scope, key string, doc ImportDocument) (json.RawMessage, error) {
	return store.Mutate(ctx, scope, core.Mutation{Operation: "map.import.apply", Target: doc.Source.Key, Key: key, Request: doc}, func(ctx context.Context, tx *sql.Tx) (any, error) {
		plan, conflicts, err := planImport(ctx, tx, scope.ProjectID, doc)
		if err != nil {
			return nil, err
		}
		if conflicts != 0 {
			return nil, &core.Error{Code: "map_import_conflict", Message: "Resolve the map import conflicts and preview again.", Status: 409, Details: map[string]any{"conflicts": conflicts}}
		}
		current, exists, err := existingImportRecord(ctx, tx, scope.ProjectID, doc.Source.Key)
		if err != nil {
			return nil, err
		}
		receipt := ImportReceipt{SourceKey: doc.Source.Key, Revision: current.Revision, Outcome: "unchanged"}
		for _, p := range plan {
			switch p.Action {
			case "create":
				receipt.Created++
			case "update":
				receipt.Updated++
			case "unchanged":
				receipt.Unchanged++
			}
		}
		if exists && receipt.Created == 0 && receipt.Updated == 0 && sameImportDocument(current.Document, doc) {
			return receipt, nil
		}
		if current.Revision == math.MaxInt64 {
			return nil, invalid()
		}
		receipt.Revision++
		receipt.Outcome = "imported"
		record := ImportRecord{SourceKey: doc.Source.Key, Revision: receipt.Revision, Document: doc, Mappings: make([]ImportMapping, 0, len(doc.Entries)), ActorID: scope.PrincipalID, WorkerID: scope.WorkerID, CreatedAt: stamp()}
		// All admissions completed before the first map write. The same owning
		// transaction helpers preserve ordinary map history and phase invariants.
		for n, item := range doc.Entries {
			p := plan[n]
			var r Receipt
			switch p.Action {
			case "create":
				r, err = createEntryTx(ctx, tx, scope, item.Entry)
			case "update":
				r, err = replaceEntryTx(ctx, tx, scope, p.EntryID, p.CurrentRevision, item.Entry)
			default:
				r = Receipt{ID: p.EntryID, Revision: p.CurrentRevision, Outcome: "unchanged"}
			}
			if err != nil {
				return nil, err
			}
			record.Mappings = append(record.Mappings, ImportMapping{item.SourceKey, r.ID, r.Revision, record.Revision})
		}
		raw, err := json.Marshal(record)
		if err != nil || len(raw) > MaxImportRecordBytes {
			return nil, invalid()
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO project_map_import_sources(project_id,source_key,revision,record_json) VALUES(?,?,?,?) ON CONFLICT(project_id,source_key) DO UPDATE SET revision=excluded.revision,record_json=excluded.record_json`, scope.ProjectID, record.SourceKey, record.Revision, string(raw)); err != nil {
			return nil, err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO project_map_import_history(project_id,source_key,revision,actor_id,worker_id,record_json,created_at) VALUES(?,?,?,?,?,?,?)`, scope.ProjectID, record.SourceKey, record.Revision, record.ActorID, record.WorkerID, string(raw), record.CreatedAt); err != nil {
			return nil, err
		}
		for _, m := range record.Mappings {
			if _, err = tx.ExecContext(ctx, `INSERT INTO project_map_import_items(project_id,source_key,item_key,entry_id,entry_revision,source_revision) VALUES(?,?,?,?,?,?) ON CONFLICT(project_id,source_key,item_key) DO UPDATE SET entry_id=excluded.entry_id,entry_revision=excluded.entry_revision,source_revision=excluded.source_revision`, scope.ProjectID, record.SourceKey, m.SourceKey, m.EntryID, m.EntryRevision, m.SourceRevision); err != nil {
				return nil, err
			}
		}
		if err = core.AppendEvent(ctx, tx, scope, "map.import_applied", doc.Source.Key, record.Revision, map[string]any{"source_key": doc.Source.Key}); err != nil {
			return nil, err
		}
		return receipt, nil
	})
}
