package historycollect

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"slices"
	"sort"
	"time"

	"github.com/benhook1013/FireController/internal/reviewprovider"
	"github.com/benhook1013/FireController/internal/strictjson"
)

func validTime(value string) bool {
	t, err := time.Parse(time.RFC3339Nano, value)
	return err == nil && !t.IsZero() && t.Year() >= 1 && t.Year() <= 9999 && len(value) <= 40
}

// validateCapture derives its position from selected source identity rather than
// trusting the retained continuation. It grants no external completeness claim.
func validateCapture(r retainedOperation, c Capture) (Position, []string, error) {
	p := Position{PullIndex: -1, Resource: c.Resource, Page: c.Page}
	for i, n := range r.Selection.PullNumbers {
		if n == c.PullNumber {
			p.PullIndex = i
			break
		}
	}
	if c.Version != 1 || c.RepositoryID != r.RepositoryID || c.RepositoryName == "" || !validTime(c.CollectedAt) || !validPosition(r.Selection, p) {
		return p, nil, storageError()
	}
	ids, err := reviewprovider.ValidateHistoryFacts(c.Resource, c.Facts)
	if err != nil || len(ids) != c.Count {
		return p, nil, storageError()
	}
	if c.Resource == "pull" {
		var pull reviewprovider.Pull
		if json.Unmarshal(c.Facts, &pull) != nil || pull.Number != c.PullNumber || pull.RepositoryID != c.RepositoryID {
			return p, nil, storageError()
		}
	}
	if c.Next != nil && c.Next.Resource == p.Resource && c.Next.PullIndex == p.PullIndex {
		if p.Resource == "pull" || p.Page == maxPage || c.Next.Page != p.Page+1 {
			return p, nil, storageError()
		}
		return p, ids, nil
	}
	next := p
	next.Page = 1
	switch p.Resource {
	case "pull":
		next.Resource = "reviews"
	case "reviews":
		next.Resource = "review_comments"
	case "review_comments":
		next.Resource = "issue_comments"
	case "issue_comments":
		next.Resource, next.PullIndex = "pull", next.PullIndex+1
	}
	if next.PullIndex == len(r.Selection.PullNumbers) {
		if c.Next != nil {
			return p, nil, storageError()
		}
	} else if c.Next == nil || *c.Next != next {
		return p, nil, storageError()
	}
	return p, ids, nil
}

// ValidateSnapshot validates all selected immutable acquisition facts and
// derived identity/checkpoint state during full read-only backup inspection.
// Ordinary reads remain bounded; this full graph check does no provider work.
func ValidateSnapshot(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `SELECT project_id,id FROM history_collections ORDER BY project_id,id`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return err
		}
		var project, id string
		if err := rows.Scan(&project, &id); err != nil {
			return err
		}
		r, err := readOperation(ctx, tx, project, id)
		if err != nil || !operationPattern.MatchString(id) || r.RepositoryID < 1 || r.actor == "" || r.worker == "" || !validTime(r.CreatedAt) || !validTime(r.UpdatedAt) {
			return storageError()
		}
		var repositoryID int64
		if err := tx.QueryRowContext(ctx, `SELECT github_repository_id FROM projects WHERE id=?`, project).Scan(&repositoryID); err != nil || repositoryID != r.RepositoryID {
			return storageError()
		}
		if r.State == "paused" {
			switch r.Reason {
			case "configuration_changed", "authorization_changed", "rate_limited", "source_invalid", "source_changed", "source_unavailable":
			default:
				return storageError()
			}
		} else if r.Reason != "" {
			return storageError()
		}
		if err := validateOperationPages(ctx, tx, r); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	var orphan bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM history_collection_pages p LEFT JOIN history_collections c ON c.project_id=p.project_id AND c.id=p.collection_id WHERE c.id IS NULL) OR EXISTS(SELECT 1 FROM history_collection_seen s LEFT JOIN history_collection_pages p ON p.project_id=s.project_id AND p.collection_id=s.collection_id AND p.id=s.page_id WHERE p.id IS NULL)`).Scan(&orphan); err != nil {
		return err
	}
	if orphan {
		return storageError()
	}
	return nil
}

func validateOperationPages(ctx context.Context, tx *sql.Tx, r retainedOperation) error {
	rows, err := tx.QueryContext(ctx, `SELECT id,sequence,pass_number,reader_fingerprint,pull_number,resource,page_number,record_count,body_sha256,body_json,created_at FROM history_collection_pages WHERE project_id=? AND collection_id=? ORDER BY sequence`, r.ProjectID, r.ID)
	if err != nil {
		return err
	}
	defer rows.Close()
	var pages, records, pass int64
	var previous *Capture
	var previousPosition Position
	var previousReader string
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return err
		}
		var item PageSummary
		var reader, body string
		if err := rows.Scan(&item.ID, &item.Sequence, &item.PassNumber, &reader, &item.PullNumber, &item.Resource, &item.Page, &item.Count, &item.SHA256, &body, &item.CreatedAt); err != nil {
			return err
		}
		hash := sha256.Sum256([]byte(body))
		var c Capture
		if !pagePattern.MatchString(item.ID) || item.Sequence != pages+1 || item.PassNumber < 1 || item.PassNumber < pass || !digestPattern.MatchString(reader) || !validTime(item.CreatedAt) || len(body) > (8<<20)+16384 || hex.EncodeToString(hash[:]) != item.SHA256 || strictjson.Decode([]byte(body), &c) != nil {
			return storageError()
		}
		position, ids, err := validateCapture(r, c)
		if err != nil || c.PullNumber != item.PullNumber || c.Resource != item.Resource || c.Page != item.Page || int64(c.Count) != item.Count {
			return storageError()
		}
		canonical, err := json.Marshal(c)
		if err != nil || !bytes.Equal(canonical, []byte(body)) {
			return storageError()
		}
		if previous == nil {
			if position != Start() {
				return storageError()
			}
		} else {
			if previous.Next == nil {
				return storageError()
			}
			expected := *previous.Next
			if item.PassNumber == pass {
				if position != expected || position.Resource != previousPosition.Resource || position.PullIndex != previousPosition.PullIndex || reader != previousReader {
					return storageError()
				}
			} else {
				expected.Page = 1
				if position != expected {
					return storageError()
				}
			}
		}
		seen, err := tx.QueryContext(ctx, `SELECT record_id FROM history_collection_seen WHERE project_id=? AND collection_id=? AND pass_number=? AND page_id=? ORDER BY record_id`, r.ProjectID, r.ID, item.PassNumber, item.ID)
		if err != nil {
			return err
		}
		actual := []string{}
		for seen.Next() {
			var id string
			if err := seen.Scan(&id); err != nil {
				seen.Close()
				return err
			}
			actual = append(actual, id)
		}
		err = seen.Err()
		seen.Close()
		if err != nil {
			return err
		}
		sort.Strings(ids)
		if !slices.Equal(ids, actual) {
			return storageError()
		}
		pages, records, pass = pages+1, records+item.Count, item.PassNumber
		previous, previousPosition, previousReader = &c, position, reader
	}
	if err := rows.Err(); err != nil {
		return err
	}
	var seenCount int64
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM history_collection_seen WHERE project_id=? AND collection_id=?`, r.ProjectID, r.ID).Scan(&seenCount); err != nil {
		return err
	}
	if pages != r.PageCount || records != r.RecordCount || seenCount != records || r.PassNumber < pass {
		return storageError()
	}
	if previous == nil {
		if r.Position != Start() || r.State == "completed" {
			return storageError()
		}
		return nil
	}
	if r.State == "completed" {
		if previous.Next != nil || r.Position != previousPosition || r.PassNumber != pass {
			return storageError()
		}
		return nil
	}
	if previous.Next == nil {
		return storageError()
	}
	expected := *previous.Next
	if r.PassNumber > pass {
		expected.Page = 1
	}
	if r.Position != expected {
		return storageError()
	}
	return nil
}
