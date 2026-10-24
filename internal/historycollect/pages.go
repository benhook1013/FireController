package historycollect

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"regexp"
	"strconv"
	"strings"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/strictjson"
)

// Kept separate from source page numbers: these are immutable local captures.
var pagePattern = regexp.MustCompile(`^history_page_[0-9a-f]{32}$`)

type PageSummary struct {
	ID         string `json:"id"`
	Sequence   int64  `json:"sequence"`
	PassNumber int64  `json:"pass_number"`
	PullNumber int64  `json:"pull_number"`
	Resource   string `json:"resource"`
	Page       int64  `json:"page"`
	Count      int64  `json:"count"`
	SHA256     string `json:"sha256"`
	CreatedAt  string `json:"created_at"`
}

type PageList struct {
	Items   []PageSummary `json:"items"`
	Cursor  string        `json:"cursor,omitempty"`
	HasMore bool          `json:"has_more"`
}

// Pages pins its first upper sequence in the signed continuation. New captures
// do not move that archive selection or trigger automatic page expansion.
func (e *Engine) Pages(ctx context.Context, scope core.Scope, id string, limit int, cursor string) (PageList, error) {
	out := PageList{Items: []PageSummary{}}
	if !operationPattern.MatchString(id) || limit < 1 || limit > 100 {
		return out, invalid()
	}
	err := e.store.Read(ctx, scope, func(ctx context.Context, tx *sql.Tx) error {
		if err := checkActor(ctx, tx, scope); err != nil {
			return err
		}
		r, err := readOperation(ctx, tx, scope.ProjectID, id)
		if err != nil {
			return err
		}
		filter := "history-pages-v1:" + id
		position, err := e.store.ParseCursor(scope, filter, cursor)
		if err != nil {
			return err
		}
		upper, after := r.PageCount, int64(0)
		if cursor != "" {
			parts := strings.Split(position, ":")
			if len(parts) != 2 {
				return invalid()
			}
			upper, err = strconv.ParseInt(parts[0], 10, 64)
			if err != nil || upper < 1 || upper > r.PageCount || strconv.FormatInt(upper, 10) != parts[0] {
				return invalid()
			}
			after, err = strconv.ParseInt(parts[1], 10, 64)
			if err != nil || after < 1 || after >= upper || strconv.FormatInt(after, 10) != parts[1] {
				return invalid()
			}
		}
		rows, err := tx.QueryContext(ctx, `SELECT id,sequence,pass_number,pull_number,resource,page_number,record_count,body_sha256,created_at FROM history_collection_pages WHERE project_id=? AND collection_id=? AND sequence>? AND sequence<=? ORDER BY sequence LIMIT ?`, scope.ProjectID, id, after, upper, limit+1)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var item PageSummary
			if err := rows.Scan(&item.ID, &item.Sequence, &item.PassNumber, &item.PullNumber, &item.Resource, &item.Page, &item.Count, &item.SHA256, &item.CreatedAt); err != nil {
				return err
			}
			if len(out.Items) == limit {
				out.HasMore = true
				out.Cursor = e.store.Cursor(scope, filter, strconv.FormatInt(upper, 10)+":"+strconv.FormatInt(out.Items[len(out.Items)-1].Sequence, 10))
				break
			}
			out.Items = append(out.Items, item)
		}
		return rows.Err()
	})
	return out, err
}

// Capture reads one deliberately selected bounded source body. The default
// operation/page list never includes provider text.
func (e *Engine) Capture(ctx context.Context, scope core.Scope, id, pageID string) (Capture, error) {
	var out Capture
	if !operationPattern.MatchString(id) || !pagePattern.MatchString(pageID) {
		return out, invalid()
	}
	err := e.store.Read(ctx, scope, func(ctx context.Context, tx *sql.Tx) error {
		if err := checkActor(ctx, tx, scope); err != nil {
			return err
		}
		r, err := readOperation(ctx, tx, scope.ProjectID, id)
		if err != nil {
			return err
		}
		var body, digest string
		var number, page, count int64
		var resource string
		err = tx.QueryRowContext(ctx, `SELECT body_json,body_sha256,pull_number,resource,page_number,record_count FROM history_collection_pages WHERE project_id=? AND collection_id=? AND id=?`, scope.ProjectID, id, pageID).Scan(&body, &digest, &number, &resource, &page, &count)
		if errors.Is(err, sql.ErrNoRows) {
			return missing()
		}
		if err != nil {
			return err
		}
		hash := sha256.Sum256([]byte(body))
		if len(body) > (8<<20)+16384 || hex.EncodeToString(hash[:]) != digest || strictjson.Decode([]byte(body), &out) != nil || out.Version != 1 {
			return storageError()
		}
		if _, _, err := validateCapture(r, out); err != nil || out.PullNumber != number || out.Resource != resource || out.Page != page || int64(out.Count) != count {
			return storageError()
		}
		return nil
	})
	return out, err
}
