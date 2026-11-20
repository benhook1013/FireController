package retention

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/benhook1013/FireController/internal/core"
)

type historyKind struct {
	members, current string
	keys             []string
}

var historyKinds = []historyKind{
	{"review_pull_memberships", "review_current_pulls", []string{"number"}},
	{"review_run_memberships", "review_current_runs", []string{"id"}},
	{"review_attempt_memberships", "review_current_attempts", []string{"source", "source_attempt_id"}},
	{"review_retired_memberships", "review_current_retired", []string{"number"}},
}

var versionTables = []string{"review_finding_versions", "review_run_versions", "review_attempt_versions", "review_pull_versions"}

type historyReader interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}
type fkPart struct{ from, to string }
type inboundLink struct {
	table   string
	columns []fkPart
}
type historyReferences map[string][]inboundLink

// Enumerate inbound references from SQLite, including composite foreign keys
// and tables not named by retention. Unknown implicit targets fail closed.
func discoverHistoryReferences(ctx context.Context, q historyReader) (historyReferences, error) {
	rows, err := q.QueryContext(ctx, `SELECT name FROM sqlite_schema WHERE type='table' AND name NOT GLOB 'sqlite_*' ORDER BY name`)
	if err != nil {
		return nil, err
	}
	var tables []string
	for rows.Next() {
		var table string
		if err = rows.Scan(&table); err != nil {
			rows.Close()
			return nil, err
		}
		tables = append(tables, table)
	}
	if err = errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	primaryKeys := make(map[string][]string)
	for _, table := range append([]string{"review_snapshots"}, versionTables...) {
		rows, err = q.QueryContext(ctx, `SELECT name FROM pragma_table_info(?) WHERE pk>0 ORDER BY pk`, table)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var name string
			if err = rows.Scan(&name); err != nil {
				rows.Close()
				return nil, err
			}
			primaryKeys[table] = append(primaryKeys[table], name)
		}
		if err = errors.Join(rows.Err(), rows.Close()); err != nil {
			return nil, err
		}
	}
	r := make(historyReferences)
	for _, table := range tables {
		rows, err = q.QueryContext(ctx, `SELECT id,seq,"table","from","to" FROM pragma_foreign_key_list(?) ORDER BY id,seq`, table)
		if err != nil {
			return nil, err
		}
		prev := -1
		for rows.Next() {
			var id, seq int
			var target, from string
			var to sql.NullString
			if err = rows.Scan(&id, &seq, &target, &from, &to); err != nil {
				rows.Close()
				return nil, err
			}
			target = strings.ToLower(target)
			columns, relevant := primaryKeys[target]
			if !relevant {
				continue
			}
			if !to.Valid {
				if seq >= len(columns) {
					rows.Close()
					return nil, errors.New("retention reference omits target columns")
				}
				to.String = columns[seq]
			}
			if id != prev {
				r[target] = append(r[target], inboundLink{table: table})
				prev = id
			}
			last := len(r[target]) - 1
			r[target][last].columns = append(r[target][last].columns, fkPart{from, to.String})
		}
		if err = errors.Join(rows.Err(), rows.Close()); err != nil {
			return nil, err
		}
	}
	return r, nil
}

func isPrunedVersion(table string) bool {
	for _, name := range versionTables {
		if name == table {
			return true
		}
	}
	return false
}

func noLinks(refs historyReferences, target, owner string, childState func(inboundLink, string) string) string {
	var terms []string
	for idx, link := range refs[target] {
		child := owner + "_" + strconv.Itoa(idx)
		var match []string
		for _, part := range link.columns {
			match = append(match, child+"."+ident(part.from)+"="+owner+"."+ident(part.to))
		}
		if childState != nil {
			state := childState(link, child)
			if state == "0" {
				continue
			}
			if state != "" {
				match = append(match, state)
			}
		}
		terms = append(terms, "NOT EXISTS (SELECT 1 FROM "+ident(link.table)+" "+child+" WHERE "+strings.Join(match, " AND ")+")")
	}
	if len(terms) == 0 {
		return "1"
	}
	return "(" + strings.Join(terms, " AND ") + ")"
}

func membershipTable(table string) bool {
	for _, k := range historyKinds {
		if k.members == table {
			return true
		}
	}
	return false
}

func aliveMembership(alias, fence string) string {
	return "(" + alias + ".end_revision IS NULL OR " + alias + ".end_revision>" + fence + ")"
}

// In addition to foreign keys, versioned records own semantic evidence:
// historical publisher identity, first native finalization (timeline counts),
// and first external Hosted capture (its origin event) and finalization proof.
// Keep only those anchors and referenced memberships, not intermediate copies.
func runDeleteCore(refs historyReferences, a, fence string, virtual bool) string {
	newer := "z.project_id=" + a + ".project_id AND z.id=" + a + ".id"
	oldestHosted := "(" + a + ".source<>'github-hosted-observer' OR EXISTS(SELECT 1 FROM review_run_versions z WHERE " + newer + " AND z.version_revision<" + a + ".version_revision))"
	firstNative := "(" + a + ".source<>'native-executor' OR " + a + ".finalized=0 OR EXISTS(SELECT 1 FROM review_run_versions z WHERE " + newer + " AND z.finalized=1 AND z.version_revision<" + a + ".version_revision))"
	lastFinal := "(" + a + ".finalized=0 OR EXISTS(SELECT 1 FROM review_run_versions z WHERE " + newer + " AND z.finalized=1 AND z.version_revision>" + a + ".version_revision))"
	lastIdentity := "EXISTS(SELECT 1 FROM review_run_versions z WHERE " + newer + " AND z.version_revision>" + a + ".version_revision)"
	links := noLinks(refs, "review_run_versions", a, func(link inboundLink, child string) string {
		if link.table == "review_finding_versions" {
			return "0"
		}
		if virtual && membershipTable(link.table) {
			return aliveMembership(child, fence)
		}
		return ""
	})
	return "(" + strings.Join([]string{a + ".version_revision<" + fence, lastIdentity, lastFinal, firstNative, oldestHosted, links}, " AND ") + ")"
}

func versionDelete(refs historyReferences, table, a, fence string, virtual bool) string {
	switch table {
	case "review_finding_versions":
		owner := "v.project_id=" + a + ".project_id AND v.id=" + a + ".run_id AND v.version_revision=" + a + ".run_version_revision AND v.pull_number=" + a + ".pull_number"
		return "(EXISTS(SELECT 1 FROM review_run_versions v WHERE " + owner + " AND " + runDeleteCore(refs, "v", fence, virtual) + ") AND " + noLinks(refs, table, a, nil) + ")"
	case "review_run_versions":
		core := runDeleteCore(refs, a, fence, virtual)
		return "(" + core + " AND " + noLinks(refs, table, a, func(link inboundLink, child string) string {
			if link.table != "review_finding_versions" {
				return "0"
			}
			if virtual {
				return "NOT " + noLinks(refs, "review_finding_versions", child, nil)
			}
			return ""
		}) + ")"
	default:
		terms := []string{a + ".version_revision<" + fence, noLinks(refs, table, a, func(link inboundLink, child string) string {
			if virtual && membershipTable(link.table) {
				return aliveMembership(child, fence)
			}
			return ""
		})}
		if table == "review_attempt_versions" {
			terms = append(terms, "EXISTS(SELECT 1 FROM review_attempt_versions z WHERE z.project_id="+a+".project_id AND z.source="+a+".source AND z.source_attempt_id="+a+".source_attempt_id AND z.version_revision>"+a+".version_revision)")
		}
		return "(" + strings.Join(terms, " AND ") + ")"
	}
}

func snapshotDelete(refs historyReferences, a, fence string, virtual bool) string {
	return a + ".revision<" + fence + " AND " + noLinks(refs, "review_snapshots", a, func(link inboundLink, child string) string {
		if !virtual {
			return ""
		}
		if membershipTable(link.table) {
			return "0"
		} // surviving interval endpoints after collapse are all >=F
		if isPrunedVersion(link.table) {
			return "NOT " + versionDelete(refs, link.table, child, fence, true)
		}
		return ""
	})
}

func pruneReviewHistory(ctx context.Context, p *pass, cutoff time.Time) error {
	refs, err := discoverHistoryReferences(ctx, p.store.DB())
	if err != nil {
		return historyFailure("schema_discovery", err)
	}
	var after string
	for {
		var project string
		err = p.store.DB().QueryRowContext(ctx, `SELECT project_id FROM review_workspace WHERE project_id>? ORDER BY project_id LIMIT 1`, after).Scan(&project)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return historyFailure("project_scan", err)
		}
		after = project
		floor, err := projectFloor(ctx, p, project, cutoff)
		if err != nil {
			return historyFailure("project_floor", err)
		}
		if floor == 0 {
			continue
		}
		if err = pruneHistoryProject(ctx, p, refs, project, floor); err != nil {
			return err
		}
	}
}

func readProjectFloor(ctx context.Context, q historyReader, project string, cutoff time.Time) (int64, error) {
	var eligible, previous int64
	key := core.ReviewHistoryFloorPrefix + project
	var version sql.NullString
	err := q.QueryRowContext(ctx, `SELECT (SELECT max(revision) FROM review_snapshots WHERE project_id=? AND `+legacyTime("recorded_at")+`<=?),(SELECT value FROM metadata WHERE key=?)`, project, stamp(cutoff), key).Scan(&version, &eligibleText{&previous})
	// A NULL aggregate means no historical cutoff snapshot, not an error.
	if err != nil {
		return 0, err
	}
	if version.Valid {
		eligible, err = strconv.ParseInt(version.String, 10, 64)
		if err != nil || eligible < 1 {
			return 0, errors.New("invalid horizon revision")
		}
	}
	return max(eligible, previous), nil
}

type eligibleText struct{ target *int64 }

func (e *eligibleText) Scan(src any) error {
	if src == nil {
		return nil
	}
	v, ok := src.(string)
	if !ok {
		return errors.New("invalid retained history floor")
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 1 || strconv.FormatInt(n, 10) != v {
		return errors.New("invalid retained history floor")
	}
	*e.target = n
	return nil
}

func projectFloor(ctx context.Context, p *pass, project string, cutoff time.Time) (int64, error) {
	if p.dryRun {
		return readProjectFloor(ctx, p.store.DB(), project, cutoff)
	}
	// The retained representation already has a committed immutable snapshot
	// at each candidate horizon. Do not write a replacement revision that
	// would change the observable archive at F.
	var floor int64
	err := p.store.BackgroundWrite(ctx, func(ctx context.Context, tx *sql.Tx) error {
		var err error
		floor, err = readProjectFloor(ctx, tx, project, cutoff)
		if err != nil || floor == 0 {
			return err
		}
		var persisted int64
		if err = tx.QueryRowContext(ctx, `SELECT s.revision FROM review_snapshots s JOIN review_workspace w USING(project_id)
 WHERE s.project_id=? AND s.revision=? AND s.revision<=w.revision`, project, floor).Scan(&persisted); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO metadata(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value WHERE metadata.value<>excluded.value`, core.ReviewHistoryFloorPrefix+project, strconv.FormatInt(persisted, 10))
		return err
	})
	return floor, err
}

func pruneHistoryProject(ctx context.Context, p *pass, refs historyReferences, project string, floor int64) error {
	fence := strconv.FormatInt(floor, 10)
	for _, k := range historyKinds {
		if err := p.delete(ctx, k.members, "project_id=? AND end_revision IS NOT NULL AND end_revision<=?", project, floor); err != nil {
			return historyFailure("membership_delete", err)
		}
	}
	if !p.dryRun {
		for _, k := range historyKinds {
			if err := shiftHistoryMemberships(ctx, p, k, project, floor); err != nil {
				return historyFailure("membership_shift", err)
			}
		}
	}
	for _, table := range versionTables {
		if err := p.delete(ctx, table, table+".project_id=? AND "+versionDelete(refs, table, table, fence, p.dryRun), project); err != nil {
			return historyFailure("version_delete", err)
		}
	}
	return historyFailure("snapshot_batch", pruneHistorySnapshots(ctx, p, refs, project, fence))
}

func shiftHistoryMemberships(ctx context.Context, p *pass, k historyKind, project string, floor int64) error {
	// One membership plus at most one current reference: count both against
	// the transaction budget. Deferred FK checking validates the atomic pair.
	limit := BatchRows / 2
	for {
		var n int64
		err := p.store.BackgroundWrite(ctx, func(ctx context.Context, tx *sql.Tx) error {
			if _, err := tx.ExecContext(ctx, `PRAGMA defer_foreign_keys=ON`); err != nil {
				return err
			}
			keys := strings.Join(k.keys, ",")
			rows, err := tx.QueryContext(ctx, "SELECT rowid,start_revision,"+keys+" FROM "+ident(k.members)+" WHERE project_id=? AND start_revision<? ORDER BY rowid LIMIT ?", project, floor, limit)
			if err != nil {
				return err
			}
			type overspan struct {
				id, start int64
				key       []any
			}
			var candidates []overspan
			for rows.Next() {
				var r overspan
				r.key = make([]any, len(k.keys))
				dest := []any{&r.id, &r.start}
				for i := range r.key {
					dest = append(dest, &r.key[i])
				}
				if err = rows.Scan(dest...); err != nil {
					rows.Close()
					return err
				}
				candidates = append(candidates, r)
			}
			if err = errors.Join(rows.Err(), rows.Close()); err != nil {
				return err
			}
			var eq []string
			for _, key := range k.keys {
				eq = append(eq, ident(key)+"=?")
			}
			for _, row := range candidates {
				condition := "project_id=? AND start_revision=? AND " + strings.Join(eq, " AND ")
				args := append([]any{floor, project, row.start}, row.key...)
				if _, err = tx.ExecContext(ctx, "UPDATE "+ident(k.current)+" SET start_revision=? WHERE "+condition, args...); err != nil {
					return err
				}
				if _, err = tx.ExecContext(ctx, "UPDATE "+ident(k.members)+" SET start_revision=? WHERE rowid=?", floor, row.id); err != nil {
					return err
				}
			}
			n = int64(len(candidates))
			return nil
		})
		if err != nil {
			return err
		}
		if n < int64(limit) {
			return nil
		}
		if err = wait(ctx, p.yield); err != nil {
			return err
		}
	}
}

func pruneHistorySnapshots(ctx context.Context, p *pass, refs historyReferences, project, fence string) error {
	where := "s.project_id=? AND " + snapshotDelete(refs, "s", fence, p.dryRun)
	eventJoin := "e.project_id=s.project_id AND e.event_type='review.observed' AND e.resource_id=s.snapshot_id AND e.revision=s.revision"
	if p.dryRun {
		for table, q := range map[string]string{
			"review_snapshots": "SELECT count(*) FROM review_snapshots s WHERE " + where,
			"events":           "SELECT count(*) FROM events e CROSS JOIN review_snapshots s ON " + eventJoin + " WHERE " + where,
		} {
			var n int64
			if err := p.store.DB().QueryRowContext(ctx, q, project).Scan(&n); err != nil {
				return err
			}
			p.deleted[table] += n
		}
		return nil
	}
	for {
		var snapshots, events int64
		err := p.store.BackgroundWrite(ctx, func(ctx context.Context, tx *sql.Tx) error {
			// Materialize the bounded owners, then scan the project's event
			// index once and probe the small owner set. A nested LEFT JOIN
			// would rescan the full project event range for every snapshot.
			q := "WITH batch AS MATERIALIZED (SELECT s.rowid AS row_id,s.project_id,s.snapshot_id,s.revision FROM review_snapshots s WHERE " + where + ` ORDER BY s.rowid LIMIT ?),
 matching AS MATERIALIZED (
   SELECT b.row_id,e.sequence FROM events e CROSS JOIN batch b
   ON e.resource_id=b.snapshot_id AND e.revision=b.revision
   WHERE e.project_id=? AND e.event_type='review.observed'
 )
 SELECT b.row_id,e.sequence FROM batch b LEFT JOIN matching e ON e.row_id=b.row_id ORDER BY b.row_id,e.sequence`
			rows, err := tx.QueryContext(ctx, q, project, BatchRows, project)
			if err != nil {
				return err
			}
			type owner struct {
				rowid  int64
				events []int64
			}
			var owners []owner
			used := 0
			for rows.Next() {
				var id int64
				var seq sql.NullInt64
				if err = rows.Scan(&id, &seq); err != nil {
					rows.Close()
					return err
				}
				if len(owners) == 0 || owners[len(owners)-1].rowid != id {
					owners = append(owners, owner{rowid: id})
					used++
				}
				if seq.Valid {
					owners[len(owners)-1].events = append(owners[len(owners)-1].events, seq.Int64)
					used++
				}
				if used > BatchRows {
					owners = owners[:len(owners)-1]
					break
				}
			}
			if err = errors.Join(rows.Err(), rows.Close()); err != nil {
				return err
			}
			if used > BatchRows && len(owners) == 0 {
				return errors.New("one history snapshot exceeds retention event batch limit")
			}
			for _, o := range owners {
				for _, seq := range o.events {
					if _, err = tx.ExecContext(ctx, "DELETE FROM events WHERE sequence=?", seq); err != nil {
						return err
					}
					events++
				}
				if _, err = tx.ExecContext(ctx, "DELETE FROM review_snapshots WHERE rowid=?", o.rowid); err != nil {
					return err
				}
				snapshots++
			}
			return nil
		})
		if err != nil {
			return fmt.Errorf("history snapshot removal: %w", err)
		}
		p.deleted["review_snapshots"] += snapshots
		p.deleted["events"] += events
		if snapshots == 0 {
			return nil
		}
		if err = wait(ctx, p.yield); err != nil {
			return err
		}
	}
}
