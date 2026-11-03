package projectmap

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"

	"github.com/benhook1013/FireController/internal/core"
)

const editorialPositionSQL = `COALESCE(json_extract(state_json,'$.position'),0)`
const editorialColumns = `SELECT id,kind,title,state,archived,revision,CASE WHEN length(CAST(state_json AS BLOB))<=? THEN state_json ELSE NULL END,` + editorialPositionSQL + ` FROM project_map_entries WHERE project_id=? AND archived=0 AND `
const editorialSamePositionQuery = editorialColumns + editorialPositionSQL + `=? AND id>? ORDER BY id LIMIT ?`
const editorialLaterPositionQuery = editorialColumns + editorialPositionSQL + `>? ORDER BY ` + editorialPositionSQL + `,id LIMIT ?`

const editorialCursorScope = "map.editorial:position-id-asc:v1"

type editorialPosition struct {
	Position int64  `json:"position"`
	ID       string `json:"id"`
}

func editorialEntries(store *core.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		scope, ok := scopeFor(store, w, r, "read", "limit", "cursor")
		if !ok {
			return
		}
		limit := 5
		if _, present := r.URL.Query()["limit"]; present {
			var err error
			limit, err = core.PageLimit(r)
			if err != nil || limit > 10 {
				core.WriteError(w, invalid())
				return
			}
		}
		cursor := r.URL.Query().Get("cursor")
		if _, present := r.URL.Query()["cursor"]; present && cursor == "" {
			core.WriteError(w, invalid())
			return
		}
		raw, err := store.ParseCursor(scope, editorialCursorScope, cursor)
		if err != nil {
			core.WriteError(w, err)
			return
		}
		after := editorialPosition{Position: -1}
		if raw != "" {
			if json.Unmarshal([]byte(raw), &after) != nil || after.Position < 0 || after.Position > 1000000 || !entryID.MatchString(after.ID) {
				core.WriteError(w, invalid())
				return
			}
			canonical, _ := json.Marshal(after)
			if string(canonical) != raw {
				core.WriteError(w, invalid())
				return
			}
		}
		result := Page[Entry]{Items: make([]Entry, 0, limit)}
		err = store.Read(r.Context(), scope, func(ctx context.Context, tx *sql.Tx) error {
			// Equality on position plus an ID seek handles a tied-position suffix;
			// the second range starts at later positions. Together they decode at most
			// limit+1 full entries, without scanning an earlier cursor prefix.
			readPage := func(query string, args ...any) error {
				rows, err := tx.QueryContext(ctx, query, args...)
				if err != nil {
					return err
				}
				defer rows.Close()
				for rows.Next() {
					var summary Summary
					var body sql.NullString
					var pos int64
					if err := rows.Scan(&summary.ID, &summary.Kind, &summary.Title, &summary.State, &summary.Archived, &summary.Revision, &body, &pos); err != nil {
						return err
					}
					if !body.Valid {
						return unavailable()
					}
					value, err := decodeEntry(body.String)
					if err != nil {
						return err
					}
					if value.ProjectID != scope.ProjectID || summary != (Summary{value.ID, value.Kind, value.Title, value.State, value.Archived, value.Revision}) || value.Archived || value.Position != pos {
						return unavailable()
					}
					result.Items = append(result.Items, value)
				}
				return rows.Err()
			}
			if after.Position >= 0 {
				if err := readPage(editorialSamePositionQuery, maxEntryBytes, scope.ProjectID, after.Position, after.ID, limit+1); err != nil {
					return err
				}
			}
			if remaining := limit + 1 - len(result.Items); remaining > 0 {
				if err := readPage(editorialLaterPositionQuery, maxEntryBytes, scope.ProjectID, after.Position, remaining); err != nil {
					return err
				}
			}
			if len(result.Items) > limit {
				result.Items = result.Items[:limit]
				result.HasMore = true
			}

			if result.HasMore {
				last := result.Items[len(result.Items)-1]
				encoded, _ := json.Marshal(editorialPosition{last.Position, last.ID})
				result.Cursor = store.Cursor(scope, editorialCursorScope, string(encoded))
			}
			return nil
		})
		respond(w, result, err, http.StatusOK)
	}
}
