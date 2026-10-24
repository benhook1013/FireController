package reviews

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviewprovider"
	"github.com/benhook1013/FireController/internal/strictjson"
)

type timelinePosition struct {
	Snapshot string `json:"snapshot"`
	Key      string `json:"key"`
	Order    int64  `json:"order"`
	ID       string `json:"id"`
}

var timelineKeyPattern = regexp.MustCompile(`^[0-9]{20}\.[0-9]{9}$`)

func decodeTimelinePosition(value string) (timelinePosition, error) {
	var p timelinePosition
	if value == "" {
		return p, nil
	}
	if strictjson.Decode([]byte(value), &p) != nil || p.Snapshot == "" || len(p.Snapshot) > 128 || p.Order < 0 || !strings.HasPrefix(p.ID, "review_run_") || len(p.ID) != len("review_run_")+32 || p.Key != "" && !timelineKeyPattern.MatchString(p.Key) {
		return p, validationError("Use the original timeline cursor.")
	}
	return p, nil
}

// Sort selected summary rows in SQL, never full finding bodies or a capped
// history prefix. The retained interval/run-order indexes bound the selected
// project/PR/snapshot lookup; chronology currently needs a temporary SQL sort.
// UTC seconds are exact integer arithmetic, independent of SQLite's floating
// point/millisecond date conversion. Fractional precision is padded to 9 digits.
// Subtract the RFC3339 offset separately so boundary offsets cannot overflow
// SQLite's supported wall-clock year range. The bias covers RFC3339 year zero.
const timelineKeySQL = `CASE WHEN timeline_time='' THEN '' ELSE
 printf('%020d',CAST(strftime('%s',substr(timeline_time,1,19)||'Z') AS INTEGER)+62167305600-
 CASE WHEN substr(timeline_time,-1)='Z' THEN 0 ELSE
 (CASE substr(timeline_time,-6,1) WHEN '-' THEN -1 ELSE 1 END)*
 (CAST(substr(timeline_time,-5,2) AS INTEGER)*3600+CAST(substr(timeline_time,-2,2) AS INTEGER)*60) END)
 ||'.'||substr((CASE WHEN substr(timeline_time,20,1)='.' THEN
 substr(timeline_time,21,length(timeline_time)-20-CASE WHEN substr(timeline_time,-1)='Z' THEN 1 ELSE 6 END)
 ELSE '' END)||'000000000',1,9) END`
const timelineColumns = `id,pull_number,source,source_run_id,channel,head_sha,reviewer,scope,outcome,attributable,complete,started_at,finished_at,found,accepted,routed,finalized,fixed,coverage_limits_json,run_order`

func serveTimelineRuns(w http.ResponseWriter, r *http.Request, store *core.Store, scope core.Scope, number int64, channel string, limit int, requested, cursor string) {
	filter := fmt.Sprintf("reviews.runs:%d:timeline:v1:instant-desc:run-order-desc:id-desc:channel=%s", number, channel)
	after, err := store.ParseCursor(scope, filter, cursor)
	if err != nil {
		core.WriteError(w, err)
		return
	}
	p, err := decodeTimelinePosition(after)
	if err != nil {
		core.WriteError(w, err)
		return
	}
	if requested != "" && p.Snapshot != "" && requested != p.Snapshot {
		core.WriteError(w, validationError("The snapshot must match the timeline cursor."))
		return
	}
	if requested == "" {
		requested = p.Snapshot
	}
	response := RunsResponse{ProjectID: scope.ProjectID, Items: make([]RunSummary, 0, limit)}
	err = store.Read(r.Context(), scope, func(ctx context.Context, tx *sql.Tx) error {
		meta, found, err := selectSnapshot(ctx, tx, scope.ProjectID, requested)
		if err != nil {
			return err
		}
		if !found {
			return notFound("The review snapshot is unavailable.")
		}
		setMetaRuns(&response, meta)
		if _, err = pullExists(ctx, tx, scope.ProjectID, meta.SnapshotID, number); err != nil {
			return err
		}
		query := `WITH selected AS (SELECT ` + timelineColumns + `,coalesce(nullif(finished_at,''),started_at) AS timeline_time FROM review_runs WHERE project_id=? AND snapshot_id=? AND pull_number=?`
		args := []any{scope.ProjectID, meta.SnapshotID, number}
		if channel != "" {
			query += ` AND channel=?`
			args = append(args, channel)
		}
		query += `),ordered AS (SELECT *,` + timelineKeySQL + ` AS timeline_key FROM selected) SELECT ` + timelineColumns + `,timeline_key FROM ordered`
		if p.Snapshot != "" {
			query += ` WHERE (timeline_key,run_order,id)<(?,?,?)`
			args = append(args, p.Key, p.Order, p.ID)
		}
		query += ` ORDER BY timeline_key DESC,run_order DESC,id DESC LIMIT ?`
		args = append(args, limit+1)
		rows, err := tx.QueryContext(ctx, query, args...)
		if err != nil {
			return err
		}
		var last timelinePosition
		for rows.Next() {
			var item RunSummary
			var attributable, complete, finalized int
			var limits, key string
			var order int64
			err = rows.Scan(&item.ID, &item.PullNumber, &item.Source, &item.SourceRunID, &item.Channel, &item.HeadSHA, &item.Reviewer, &item.Scope, &item.Outcome, &attributable, &complete, &item.StartedAt, &item.FinishedAt, &item.Found, &item.Accepted, &item.Routed, &finalized, &item.Fixed, &limits, &order, &key)
			if err != nil {
				rows.Close()
				return err
			}
			if len(response.Items) == limit {
				response.HasMore = true
				break
			}
			item.Attributable = attributable != 0
			item.Complete = complete != 0
			item.Finalized = finalized != 0
			if err = strictStoredJSON(limits, &item.CoverageLimits); err != nil {
				rows.Close()
				return err
			}
			response.Items = append(response.Items, item)
			last = timelinePosition{meta.SnapshotID, key, order, item.ID}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		if response.HasMore {
			b, _ := json.Marshal(last)
			response.Cursor = store.Cursor(scope, filter, string(b))
		}
		return enrichTimelineSeverity(ctx, tx, scope.ProjectID, meta.SnapshotID, number, response.Items)
	})
	if err != nil {
		core.WriteError(w, err)
		return
	}
	core.WriteJSON(w, http.StatusOK, response)
}

func displayBucket(severity string) string {
	switch severity {
	case "critical":
		return "critical"
	case "high", "major":
		return "major"
	case "medium", "low", "minor":
		return "minor"
	case "info", "trivial":
		return "trivial"
	default:
		return "unknown"
	}
}
func derivedHostedSeverity(detail string) string {
	if len(detail) > 4096 {
		detail = detail[:4096]
	}
	severity := reviewprovider.HostedDisplaySeverity(detail)
	if oneOf(severity, "critical", "high", "medium", "low", "info") {
		return severity
	}
	return "unknown"
}
func addFindingDisplaySeverity(response *RunDetailResponse) {
	if response.Run.Channel != "hosted" {
		return
	}
	for i := range response.Findings {
		f := &response.Findings[i]
		if f.Severity == "unknown" || f.Severity == "" {
			if severity := derivedHostedSeverity(f.Detail); severity != "unknown" {
				f.DisplaySeverity = severity
			}
		}
	}
}

// Fetch only this selected page's scalar finding inventory, with a byte-bounded
// detail prefix exclusively for Hosted findings lacking scalar severity.
func enrichTimelineSeverity(ctx context.Context, tx *sql.Tx, project, snapshot string, number int64, items []RunSummary) error {
	if len(items) == 0 {
		return nil
	}
	args := []any{}
	hosted := []string{}
	ids := []string{}
	index := map[string]int{}
	for i := range items {
		ids = append(ids, "?")
		index[items[i].ID] = i
		if items[i].Channel == "hosted" {
			hosted = append(hosted, "?")
			args = append(args, items[i].ID)
		}
	}
	prefix := `x''`
	if len(hosted) > 0 {
		prefix = `CASE WHEN severity IN ('','unknown') AND disposition='accepted' AND run_id IN (` + strings.Join(hosted, ",") + `) THEN substr(CAST(detail AS BLOB),1,4096) ELSE x'' END`
	}
	query := `SELECT run_id,severity,disposition,` + prefix + ` FROM review_findings WHERE project_id=? AND snapshot_id=? AND pull_number=? AND run_id IN (` + strings.Join(ids, ",") + `)`
	args = append(args, project, snapshot, number)
	for _, item := range items {
		args = append(args, item.ID)
	}
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	totals := make([]int64, len(items))
	accepted := make([]int64, len(items))
	buckets := make([]AcceptedSeverity, len(items))
	for rows.Next() {
		var id, severity, disposition string
		var detail []byte
		if err = rows.Scan(&id, &severity, &disposition, &detail); err != nil {
			return err
		}
		i := index[id]
		totals[i]++
		if disposition != "accepted" {
			continue
		}
		accepted[i]++
		bucket := displayBucket(severity)
		if bucket == "unknown" && items[i].Channel == "hosted" {
			bucket = displayBucket(derivedHostedSeverity(string(detail)))
		}
		switch bucket {
		case "critical":
			buckets[i].Critical++
		case "major":
			buckets[i].Major++
		case "minor":
			buckets[i].Minor++
		case "trivial":
			buckets[i].Trivial++
		default:
			buckets[i].Unknown++
		}
	}
	if err = rows.Err(); err != nil {
		return err
	}
	for i := range items {
		if items[i].Complete && totals[i] == items[i].Found && accepted[i] == items[i].Accepted {
			buckets[i].Complete = true
			items[i].AcceptedSeverity = &buckets[i]
		} else {
			items[i].AcceptedSeverity = &AcceptedSeverity{Complete: false}
		}
	}
	return nil
}
