package reviewexec

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/benhook1013/FireController/internal/strictjson"
)

// ValidateSelectionSnapshot checks retained finite-run bindings without current
// grants or provider access. Revoked historical actors remain valid provenance;
// no retained selection itself supplies captured review or round credit.
func ValidateSelectionSnapshot(ctx context.Context, q EvidenceQuery) error {
	if err := validateSelectionPhysical(ctx, q); err != nil {
		return err
	}
	orderColumn, err := operationOrderColumn(ctx, q)
	if err != nil {
		return err
	}
	var bad bool
	// Storage classes and empty keys are checked before scanned string keysets.
	if err := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM review_selections s LEFT JOIN projects p ON p.id=s.project_id LEFT JOIN workers w ON w.project_id=s.project_id AND w.id=s.worker_id WHERE p.id IS NULL OR w.id IS NULL OR NOT EXISTS(SELECT 1 FROM credentials c WHERE c.principal_id=s.actor_id))`).Scan(&bad); err != nil {
		return err
	}
	if bad {
		return runUnavailable()
	}
	last := ""
	for {
		rows, err := q.QueryContext(ctx, `SELECT id,project_id FROM review_selections WHERE id>? ORDER BY id LIMIT 64`, last)
		if err != nil {
			return err
		}
		type key struct{ id, project string }
		page := []key{}
		for rows.Next() {
			var k key
			if err = rows.Scan(&k.id, &k.project); err != nil {
				break
			}
			page = append(page, k)
		}
		if err == nil {
			err = rows.Err()
		}
		rows.Close()
		if err != nil {
			return err
		}
		for _, k := range page {
			var invalidChild bool
			if err = q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM review_operations WHERE id=(SELECT operation_id FROM review_selections WHERE id=?) AND (typeof(id)!='text' OR typeof(project_id)!='text' OR typeof(actor_id)!='text' OR typeof(worker_id)!='text' OR typeof(configuration_revision)!='integer' OR typeof(policy_revision)!='integer' OR typeof(pull_number)!='integer' OR typeof(revision)!='integer' OR typeof(capability_fingerprint)!='text' OR typeof(channel)!='text' OR typeof(head_sha)!='text' OR typeof(state)!='text' OR typeof(reason)!='text' OR typeof(attempt_id)!='text' OR typeof(created_at)!='text' OR typeof(updated_at)!='text' OR length(CAST(id AS BLOB))>128 OR length(CAST(project_id AS BLOB))>128 OR length(CAST(actor_id AS BLOB))>128 OR length(CAST(worker_id AS BLOB))>128 OR length(CAST(capability_fingerprint AS BLOB))>64 OR length(CAST(channel AS BLOB))>128 OR length(CAST(head_sha AS BLOB))>40 OR length(CAST(state AS BLOB))>128 OR length(CAST(reason AS BLOB))>128 OR length(CAST(attempt_id AS BLOB))>128 OR length(CAST(created_at AS BLOB))>64 OR length(CAST(updated_at AS BLOB))>64))`, k.id).Scan(&invalidChild); err != nil {
				return err
			}
			if invalidChild {
				return runUnavailable()
			}
			if orderColumn != "0" {
				if err = q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM review_operations WHERE id=(SELECT operation_id FROM review_selections WHERE id=?) AND (typeof(allow_out_of_order)!='integer' OR allow_out_of_order!=0))`, k.id).Scan(&invalidChild); err != nil {
					return err
				}
				if invalidChild {
					return runUnavailable()
				}
			}
			r, err := readRun(ctx, q, k.project, k.id)
			if err != nil {
				return err
			}
			var configurationBody, policyBody string
			if err = q.QueryRowContext(ctx, `SELECT CASE WHEN typeof(body_json)='text' AND length(CAST(body_json AS BLOB))<=16384 THEN body_json END FROM review_execution_config WHERE project_id=? AND revision=?`, r.ProjectID, r.ConfigurationRevision).Scan(&configurationBody); err != nil {
				return runUnavailable()
			}
			var c Configuration
			if strictjson.Decode([]byte(configurationBody), &c) != nil || validateConfigurationShape(c) != nil || *c.ExpectedRevision != r.ConfigurationRevision-1 || c.Paused || !slices.Contains(c.Channels, r.Channel) {
				return runUnavailable()
			}
			if err = q.QueryRowContext(ctx, `SELECT CASE WHEN typeof(body_json)='text' AND length(CAST(body_json AS BLOB))<=1024 THEN body_json END FROM review_policy_config WHERE project_id=? AND revision=?`, r.ProjectID, r.PolicyRevision).Scan(&policyBody); err != nil {
				return runUnavailable()
			}
			var p Policy
			if strictjson.Decode([]byte(policyBody), &p) != nil || !validPolicy(p) || slices.Contains(p.PausedChannels, r.Channel) {
				return runUnavailable()
			}
			if r.State == "selected" {
				o, err := readOperation(ctx, q, r.ProjectID, r.OperationID)
				if err != nil {
					return err
				}
				if o.ID != r.OperationID || !sha.MatchString(o.HeadSHA) || o.Revision < 1 || !slices.Contains([]string{"queued", "preparing", "posting", "active", "uncertain", "held", "failed", "completed", "cancelled", "rate_limited"}, o.State) || !roundTime(o.CreatedAt) || !roundTime(o.UpdatedAt) || o.ProjectID != r.ProjectID || o.ActorID != r.ActorID || o.WorkerID != r.WorkerID || o.Channel != r.Channel || o.ConfigurationRevision != r.ConfigurationRevision || o.PolicyRevision != r.PolicyRevision || o.CapabilityFingerprint != r.CapabilityFingerprint || !slices.Contains(c.Queue, o.PullNumber) || r.ExpectedPullNumber != 0 && r.ExpectedPullNumber != o.PullNumber {
					return runUnavailable()
				}
			}
			last = k.id
		}
		if len(page) < 64 {
			return nil
		}
	}
}

func validateSelectionPhysical(ctx context.Context, q EvidenceQuery) error {
	expected := map[string]string{}
	for _, s := range SelectionSchemaSQL() {
		fields := strings.Fields(s)
		if len(fields) < 3 {
			return runUnavailable()
		}
		nameIndex := 2
		if fields[1] == "UNIQUE" {
			nameIndex = 3
		}
		if len(fields) <= nameIndex {
			return runUnavailable()
		}
		expected[strings.Split(fields[nameIndex], "(")[0]] = strings.TrimSuffix(strings.TrimSpace(s), ";")
	}
	rows, err := q.QueryContext(ctx, `SELECT CASE WHEN length(name)<=128 THEN name END,CASE WHEN length(CAST(sql AS BLOB))<=16384 THEN sql END FROM sqlite_schema WHERE tbl_name='review_selections' AND (type='table' OR sql IS NOT NULL) LIMIT 4`)
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	for rows.Next() {
		var name, body string
		if err = rows.Scan(&name, &body); err != nil {
			break
		}
		if want, ok := expected[name]; !ok || seen[name] || want != strings.TrimSuffix(strings.TrimSpace(body), ";") {
			err = runUnavailable()
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
		return runUnavailable()
	}
	checks := []string{"id=''", "project_id=''", "actor_id=''", "worker_id=''", "(operation_id IS NOT NULL AND (typeof(operation_id)!='text' OR operation_id='' OR length(CAST(operation_id AS BLOB))>128))"}
	for _, col := range strings.Fields("configuration_revision policy_revision expected_pull_number revision") {
		checks = append(checks, "typeof("+col+")!='integer'")
	}
	for _, col := range strings.Fields("id project_id actor_id worker_id channel capability_fingerprint state reason created_at updated_at") {
		limit := 128
		if col == "created_at" || col == "updated_at" {
			limit = 64
		}
		checks = append(checks, fmt.Sprintf("typeof(%s)!='text' OR length(CAST(%s AS BLOB))>%d", col, col, limit))
	}
	var bad bool
	if err = q.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM review_selections WHERE "+strings.Join(checks, " OR ")+")").Scan(&bad); err != nil {
		return err
	}
	if bad {
		return runUnavailable()
	}
	return nil
}
