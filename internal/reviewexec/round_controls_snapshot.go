package reviewexec

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/benhook1013/FireController/internal/strictjson"
)

// ValidateRoundSnapshot checks the retained acceptance graph, native completion
// authority and derived progress. It is maintenance validation, not a per-read
// scan or a defense against coherent rewriting of every authoritative row.
func ValidateRoundSnapshot(ctx context.Context, q EvidenceQuery) error {
	var version int
	if err := q.QueryRowContext(ctx, `SELECT value FROM metadata WHERE key='schema:review_rounds'`).Scan(&version); err != nil {
		return err
	}
	if version != 1 && version != 2 && version != 3 {
		return roundUnavailable()
	}
	if err := validateRoundPhysicalVersion(ctx, q, version); err != nil {
		return err
	}
	for _, query := range []string{
		`SELECT EXISTS(SELECT 1 FROM review_round_changes h LEFT JOIN review_round_controls c USING(project_id,pull_number,channel) WHERE c.project_id IS NULL)`,
		`SELECT EXISTS(SELECT 1 FROM review_round_operation_bindings b LEFT JOIN review_operations o ON o.id=b.operation_id LEFT JOIN review_round_controls c ON c.project_id=b.project_id AND c.pull_number=b.pull_number AND c.channel=b.channel LEFT JOIN review_round_changes g ON g.project_id=b.project_id AND g.pull_number=b.pull_number AND g.channel=b.channel AND g.revision=b.grant_revision AND g.epoch=b.epoch WHERE o.id IS NULL OR c.project_id IS NULL OR o.project_id!=b.project_id OR o.pull_number!=b.pull_number OR o.channel!=b.channel OR g.action IS NULL OR g.action!='grant')`,
		`SELECT EXISTS(SELECT 1 FROM review_round_completions c LEFT JOIN review_round_operation_bindings b ON b.operation_id=c.operation_id WHERE b.operation_id IS NULL OR b.project_id!=c.project_id OR b.pull_number!=c.pull_number OR b.channel!=c.channel OR b.epoch!=c.epoch)`,
	} {
		var bad bool
		if err := q.QueryRowContext(ctx, query).Scan(&bad); err != nil {
			return err
		}
		if bad {
			return roundUnavailable()
		}
	}
	project, channel := "", ""
	var pull int64
	for {
		rows, err := q.QueryContext(ctx, `SELECT project_id,pull_number,channel FROM review_round_controls WHERE (project_id,pull_number,channel)>(?,?,?) ORDER BY project_id,pull_number,channel LIMIT 64`, project, pull, channel)
		if err != nil {
			return err
		}
		type scope struct {
			project, channel string
			pull             int64
		}
		page := []scope{}
		for rows.Next() {
			var v scope
			if err = rows.Scan(&v.project, &v.pull, &v.channel); err != nil {
				break
			}
			page = append(page, v)
		}
		if err == nil {
			err = rows.Err()
		}
		rows.Close()
		if err != nil {
			return err
		}
		for _, v := range page {
			if err := validateRoundHistory(ctx, q, v.project, v.pull, v.channel, version); err != nil {
				return err
			}
			project, pull, channel = v.project, v.pull, v.channel
		}
		if len(page) < 64 {
			break
		}
	}
	if err := validateRoundBindings(ctx, q); err != nil {
		return err
	}
	return validateRoundAdoptions(ctx, q)
}

// Physical validation precedes keyset traversal: SQLite TEXT affinity alone
// permits BLOB keys that cannot be resumed using a scanned Go string cursor.
func validateRoundPhysical(ctx context.Context, q EvidenceQuery) error {
	return validateRoundPhysicalVersion(ctx, q, 3)
}
func validateRoundPhysicalVersion(ctx context.Context, q EvidenceQuery, version int) error {
	expected := map[string]string{}
	definitions := RoundSchemaSQL()
	if version == 1 {
		definitions = RoundSchemaV1SQL()
	} else if version == 2 {
		definitions = RoundSchemaV2SQL()
	}
	for _, statement := range definitions {
		fields := strings.Fields(statement)
		if len(fields) < 3 {
			return roundUnavailable()
		}
		name := strings.Split(fields[2], "(")[0]
		expected[name] = strings.TrimSuffix(strings.TrimSpace(statement), ";")
	}
	rows, err := q.QueryContext(ctx, `SELECT CASE WHEN length(name)<=128 THEN name END,CASE WHEN length(CAST(sql AS BLOB))<=16384 THEN sql END FROM sqlite_schema WHERE (tbl_name IN ('review_round_controls','review_round_changes','review_round_operation_bindings','review_round_completions') AND (type='table' OR sql IS NOT NULL)) OR name='review_round_operations_scope' LIMIT 7`)
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	for rows.Next() {
		var name, definition string
		if err = rows.Scan(&name, &definition); err != nil {
			break
		}
		want, ok := expected[name]
		if !ok || seen[name] || want != strings.TrimSuffix(strings.TrimSpace(definition), ";") {
			err = roundUnavailable()
			break
		}
		seen[name] = true
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return err
	}
	if len(seen) != len(expected) {
		return roundUnavailable()
	}
	for _, table := range []struct{ name, integers, text string }{
		{"review_round_controls", "pull_number revision epoch grant_revision closed minimum maximum taper used dry tapered", "project_id actor_id created_at updated_at channel"},
		{"review_round_changes", "pull_number revision epoch", "project_id channel action body_json worker_id"},
		{"review_round_operation_bindings", "pull_number epoch grant_revision", "operation_id project_id channel"},
		{"review_round_completions", "pull_number epoch ordinal accepted", "operation_id evidence_id project_id channel completed_at"},
	} {
		if table.name == "review_round_controls" && version >= 2 {
			table.integers += " initial_dry initially_tapered"
			table.text += " origin_source origin_key origin_sha256"
		}
		if table.name == "review_round_controls" && version >= 3 {
			table.integers += " maximum_unbounded"
		}
		checks := []string{}
		for _, column := range strings.Fields(table.integers) {
			checks = append(checks, "typeof("+column+")!='integer'")
		}
		for _, column := range strings.Fields(table.text) {
			if column == "operation_id" || column == "evidence_id" || column == "project_id" {
				checks = append(checks, column+"=''")
			}
			limit := 128
			if column == "origin_source" || column == "origin_key" {
				limit = 200
			}
			if column == "body_json" {
				limit = 4096
			}
			if column == "created_at" || column == "updated_at" || column == "completed_at" {
				limit = 64
			}
			checks = append(checks, fmt.Sprintf("typeof(%s)!='text' OR length(CAST(%s AS BLOB))>%d", column, column, limit))
		}
		var bad bool
		if err := q.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM "+table.name+" WHERE "+strings.Join(checks, " OR ")+")").Scan(&bad); err != nil {
			return err
		}
		if bad {
			return roundUnavailable()
		}
	}
	return nil
}

type roundProgress struct {
	dry     int64
	tapered bool
	at      string
}

func roundCompletionAuthority(ctx context.Context, q EvidenceQuery, project, operationID string) (string, int64, error) {
	op, err := readOperation(ctx, q, project, operationID)
	if err != nil {
		return "", 0, err
	}
	if op.State != "completed" {
		return "", 0, nil
	}
	proof, err := deriveComparisonProof(ctx, q, project, operationID)
	if err != nil || proof.evidence == "" {
		return "", 0, err
	}
	evidence, state, err := loadReviewAdjudication(ctx, q, project, proof.evidence)
	if err != nil {
		return "", 0, err
	}
	if !state.Finalized {
		return "", 0, nil
	}
	if state.FrozenCounts == nil || evidence.OperationID != operationID || evidence.AttemptID != op.AttemptID || !evidence.Capture.Review.Complete || evidence.Capture.Review.Outcome != "completed" {
		return "", 0, roundUnavailable()
	}
	return evidence.ID, state.FrozenCounts.Accepted, nil
}

// Only scalar progress for one grant is retained, including unbounded grants. Capture
// bodies are loaded one operation at a time by the existing authority validator.
type roundEpochState struct {
	used   int64
	latest roundProgress
}

func roundEpochProgress(ctx context.Context, q EvidenceQuery, grant RoundControl, action string, through int64) (roundEpochState, error) {
	progress := roundEpochState{latest: roundProgress{dry: grant.InitialDry, tapered: grant.InitiallyTapered && grant.Minimum == 0}}
	if through == 0 {
		return progress, nil
	}
	last := int64(0)
	for {
		rows, err := q.QueryContext(ctx, `SELECT operation_id,evidence_id,ordinal,accepted,completed_at FROM review_round_completions WHERE project_id=? AND pull_number=? AND channel=? AND epoch=? AND ordinal>? ORDER BY ordinal LIMIT 64`, grant.ProjectID, grant.PullNumber, grant.Channel, grant.Epoch, last)
		if err != nil {
			return roundEpochState{}, err
		}
		type completion struct {
			op, evidence, at  string
			ordinal, accepted int64
		}
		page := []completion{}
		for rows.Next() {
			var v completion
			if err = rows.Scan(&v.op, &v.evidence, &v.ordinal, &v.accepted, &v.at); err != nil {
				break
			}
			page = append(page, v)
		}
		if err == nil {
			err = rows.Err()
		}
		rows.Close()
		if err != nil {
			return roundEpochState{}, err
		}
		for _, v := range page {
			if action != "grant" || v.ordinal != last+1 || v.ordinal > roundSafeInteger || !grant.MaximumUnbounded && v.ordinal > grant.Maximum || !identifier.MatchString(v.op) || !identifier.MatchString(v.evidence) || !roundTime(v.at) {
				return roundEpochState{}, roundUnavailable()
			}
			evidence, accepted, err := roundCompletionAuthority(ctx, q, grant.ProjectID, v.op)
			if err != nil {
				return roundEpochState{}, err
			}
			if evidence != v.evidence || accepted != v.accepted {
				return roundEpochState{}, roundUnavailable()
			}
			next := progress.latest
			if !next.tapered {
				if accepted == 0 {
					if next.dry >= roundSafeInteger {
						return roundEpochState{}, roundUnavailable()
					}
					next.dry++
				} else {
					next.dry = 0
				}
				if v.ordinal >= grant.Minimum && (grant.InitiallyTapered || next.dry >= grant.Taper) {
					next.tapered = true
				}
			}
			next.at = v.at
			progress = roundEpochState{used: v.ordinal, latest: next}
			last = v.ordinal
			if through >= 0 && last == through {
				return progress, nil
			}
		}
		if len(page) < 64 {
			break
		}
	}
	return progress, nil
}

func validateRoundHistory(ctx context.Context, q EvidenceQuery, project string, pull int64, channel string, version int) error {
	current, err := readRoundRecordVersion(ctx, q, project, pull, channel, version)
	if err != nil {
		return err
	}
	var last RoundControl
	var revision, grantRevision int64
	var progress roundEpochState
	var grant RoundControl
	var grantAction string
	for {
		rows, err := q.QueryContext(ctx, `SELECT revision,epoch,action,body_json,worker_id FROM review_round_changes WHERE project_id=? AND pull_number=? AND channel=? AND revision>? ORDER BY revision LIMIT 64`, project, pull, channel, revision)
		if err != nil {
			return err
		}
		type change struct {
			revision, epoch      int64
			action, body, worker string
		}
		page := []change{}
		for rows.Next() {
			var v change
			if err = rows.Scan(&v.revision, &v.epoch, &v.action, &v.body, &v.worker); err != nil {
				break
			}
			page = append(page, v)
		}
		if err == nil {
			err = rows.Err()
		}
		rows.Close()
		if err != nil {
			return err
		}
		for _, v := range page {
			var c RoundControl
			if v.revision != revision+1 || strictjson.Decode([]byte(v.body), &c) != nil || encoded(c) != v.body || !validRoundControl(c) || version < 3 && c.MaximumUnbounded || c.ProjectID != project || c.PullNumber != pull || c.Channel != channel || c.Revision != v.revision || c.Epoch != v.epoch || v.worker != "" && !identifier.MatchString(v.worker) {
				return roundUnavailable()
			}
			if revision == 0 {
				if c.Epoch != 1 {
					return roundUnavailable()
				}
			} else if c.CreatedAt != last.CreatedAt {
				return roundUnavailable()
			}
			if v.action == "grant" {
				if (!hasRoundOrigin(c) && c.Closed) || c.Used != 0 || c.Dry != c.InitialDry || c.Tapered != (c.InitiallyTapered && c.Minimum == 0) || revision > 0 && (c.Epoch != last.Epoch+1 || hasRoundOrigin(c)) {
					return roundUnavailable()
				}
				grantRevision = c.Revision
				grant, grantAction = c, v.action
				progress, err = roundEpochProgress(ctx, q, c, v.action, -1)
			} else if v.action == "close" {
				if !c.Closed {
					return roundUnavailable()
				}
				if revision == 0 {
					if c.Minimum != 0 || c.Maximum != 1 {
						return roundUnavailable()
					}
					grantRevision = 1
					grant, grantAction = c, v.action
					progress, err = roundEpochProgress(ctx, q, c, v.action, -1)
				} else if c.Epoch != last.Epoch || c.Minimum != last.Minimum || c.Maximum != last.Maximum || c.MaximumUnbounded != last.MaximumUnbounded || c.Taper != last.Taper || c.InitialDry != last.InitialDry || c.InitiallyTapered != last.InitiallyTapered || c.OriginSource != last.OriginSource || c.OriginKey != last.OriginKey || c.OriginSHA256 != last.OriginSHA256 {
					return roundUnavailable()
				}
				if err == nil {
					checkpoint, readErr := roundEpochProgress(ctx, q, grant, grantAction, c.Used)
					if readErr != nil {
						return readErr
					}
					if c.Used > progress.used || c.Used != checkpoint.used || c.Used < last.Used || c.Dry != checkpoint.latest.dry || c.Tapered != checkpoint.latest.tapered {
						return roundUnavailable()
					}
				}
			} else {
				return roundUnavailable()
			}
			if err != nil {
				return err
			}
			revision, last = c.Revision, c
		}
		if len(page) < 64 {
			break
		}
	}
	if revision == 0 || current.Revision != revision || current.Epoch != last.Epoch || current.grantRevision != grantRevision {
		return roundUnavailable()
	}
	used := progress.used
	if last.Used > used {
		return roundUnavailable()
	}
	expected := last
	expected.Used, expected.Dry, expected.Tapered = used, progress.latest.dry, progress.latest.tapered
	if used > last.Used {
		expected.UpdatedAt = progress.latest.at
	}
	if current.RoundControl != expected {
		return roundUnavailable()
	}
	return nil
}

func validateRoundBindings(ctx context.Context, q EvidenceQuery) error {
	last := ""
	for {
		rows, err := q.QueryContext(ctx, `SELECT operation_id,project_id,pull_number,channel,epoch,grant_revision FROM review_round_operation_bindings WHERE operation_id>? ORDER BY operation_id LIMIT 64`, last)
		if err != nil {
			return err
		}
		type binding struct {
			id, project, channel string
			pull, epoch, grant   int64
		}
		page := []binding{}
		for rows.Next() {
			var b binding
			if err = rows.Scan(&b.id, &b.project, &b.pull, &b.channel, &b.epoch, &b.grant); err != nil {
				break
			}
			page = append(page, b)
		}
		if err == nil {
			err = rows.Err()
		}
		rows.Close()
		if err != nil {
			return err
		}
		for _, b := range page {
			if !identifier.MatchString(b.id) || !roundScope(b.project, b.pull, b.channel) {
				return roundUnavailable()
			}
			if _, err := readRoundGrant(ctx, q, b.project, b.pull, b.channel, b.grant, b.epoch); err != nil {
				return err
			}
			expected, _, err := roundCompletionAuthority(ctx, q, b.project, b.id)
			if err != nil {
				return err
			}
			var actual string
			err = q.QueryRowContext(ctx, `SELECT evidence_id FROM review_round_completions WHERE operation_id=?`, b.id).Scan(&actual)
			if expected == "" {
				if !errors.Is(err, sql.ErrNoRows) {
					if err != nil {
						return err
					}
					return roundUnavailable()
				}
			} else if err != nil || actual != expected {
				if err != nil {
					return err
				}
				return roundUnavailable()
			}
			last = b.id
		}
		if len(page) < 64 {
			break
		}
	}
	return nil
}
