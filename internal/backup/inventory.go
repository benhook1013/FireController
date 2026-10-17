package backup

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/benhook1013/FireController/internal/reviewexec"
	"github.com/benhook1013/FireController/internal/strictjson"
)

var cliName = regexp.MustCompile(`^review_attempt_[0-9a-f]{32}\.json$`)
var hostedName = regexp.MustCompile(`^review_attempt_[0-9a-f]{32}-hosted-[0-9a-f]{64}\.json$`)

func artifactPath(path string) (string, int64, bool) {
	parts := strings.Split(path, "/")
	if len(parts) != 2 {
		return "", 0, false
	}
	switch parts[0] {
	case "review-captures":
		if cliName.MatchString(parts[1]) {
			return strings.TrimSuffix(parts[1], ".json"), maxCLICaptureBytes, true
		}
	case "hosted-review-captures":
		if hostedName.MatchString(parts[1]) {
			return parts[1][:len("review_attempt_")+32], maxHostedCaptureBytes, true
		}
	}
	return "", 0, false
}

// Each managed-directory walk is bounded to maxArtifacts entries, including
// ignored temporary/unrelated names; the combined retained inventory has the
// same bound. Database bytes and artifact sums stream without a deployment cap.
func copyInventory(ctx context.Context, source, destination string) ([]Artifact, error) {
	items := make([]Artifact, 0)
	for _, name := range []string{"review-captures", "hosted-review-captures"} {
		dir := filepath.Join(source, name)
		_, err := os.Lstat(dir)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil || privateDirectory(dir, false) != nil {
			return nil, invalid()
		}
		f, err := os.Open(dir)
		if err != nil {
			return nil, unavailable()
		}
		entries := 0
		for {
			batch, e := f.ReadDir(512)
			if e != nil && e != io.EOF {
				f.Close()
				return nil, unavailable()
			}
			for _, entry := range batch {
				entries++
				if entries > maxArtifacts || checkContext(ctx) != nil {
					f.Close()
					return nil, unavailable()
				}
				info, err := entry.Info()
				if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
					f.Close()
					return nil, invalid()
				}
				relative := name + "/" + entry.Name()
				_, limit, recognized := artifactPath(relative)
				if !recognized {
					continue
				}
				if len(items) >= maxArtifacts {
					f.Close()
					return nil, invalid()
				}
				if privateDirectory(filepath.Join(destination, name), true) != nil {
					f.Close()
					return nil, invalid()
				}
				a, err := copyPrivate(ctx, filepath.Join(dir, entry.Name()), filepath.Join(destination, name, entry.Name()), limit)
				if err != nil {
					f.Close()
					return nil, err
				}
				a.Path = relative
				if name == "hosted-review-captures" && !strings.HasSuffix(relative, "-"+a.SHA256+".json") {
					f.Close()
					return nil, invalid()
				}
				items = append(items, a)
			}
			if e == io.EOF {
				break
			}
		}
		f.Close()
		if len(items) > 0 {
			if _, err := os.Lstat(filepath.Join(destination, name)); err == nil {
				if err = syncDirectory(filepath.Join(destination, name)); err != nil {
					return nil, err
				}
			}
		}
	}
	return items, nil
}
func checkBundleFiles(ctx context.Context, root string, expected map[string]bool) error {
	directory, err := os.Open(root)
	if err != nil {
		return unavailable()
	}
	var entries []os.DirEntry
	for {
		batch, e := directory.ReadDir(6)
		entries = append(entries, batch...)
		if len(entries) > 5 {
			directory.Close()
			return invalid()
		}
		if e == io.EOF {
			break
		}
		if e != nil {
			directory.Close()
			return unavailable()
		}
		if checkContext(ctx) != nil {
			directory.Close()
			return unavailable()
		}
	}
	directory.Close()
	found := map[string]bool{}
	for _, entry := range entries {
		if checkContext(ctx) != nil {
			return unavailable()
		}
		name := entry.Name()
		if name == "review-captures" || name == "hosted-review-captures" {
			dir := filepath.Join(root, name)
			if privateDirectory(dir, false) != nil {
				return invalid()
			}
			f, err := os.Open(dir)
			if err != nil {
				return unavailable()
			}
			count := 0
			for {
				batch, e := f.ReadDir(512)
				if e != nil && e != io.EOF {
					f.Close()
					return unavailable()
				}
				for _, child := range batch {
					count++
					if count > maxArtifacts || checkContext(ctx) != nil {
						f.Close()
						return invalid()
					}
					relative := name + "/" + child.Name()
					if !expected[relative] {
						f.Close()
						return invalid()
					}
					if _, err = privateFile(filepath.Join(dir, child.Name())); err != nil {
						f.Close()
						return err
					}
					found[relative] = true
				}
				if e == io.EOF {
					break
				}
			}
			f.Close()
		} else {
			if !expected[name] {
				return invalid()
			}
			if _, err = privateFile(filepath.Join(root, name)); err != nil {
				return err
			}
			found[name] = true
		}
	}
	for path := range expected {
		if !found[path] {
			return invalid()
		}
	}
	return nil
}

type sqlReader interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}
type tableVersion struct {
	domain  string
	since   int
	columns string
}

// Fixed application schema bounds are intentionally independent of migration
// execution. Inspection never opens a Store or installs/rewrites a schema.
var supportedDomains = map[string]int{"jobs": 2, "inbox": 1, "reviews": 3, "review_policy": 1, "review_execution": 6, "review_execution_indexes": 1, "review_evidence": 3, "review_adjudication": 2, "review_source_refresh": 3, "review_checkpoint_publication": 1, "managed_backups": 1, "managed_backup_publications": 1, "recovery_history": 1}
var tables = map[string]tableVersion{
	"restore_history":                  {"recovery_history", 1, "instance_id body_json"},
	"managed_backup_publication_holds": {"managed_backup_publications", 1, "bundle_id destination source_instance_id manifest_sha256 summary_json"},
	"managed_backup_state":             {"managed_backups", 1, "singleton body_json"},
	"managed_backup_bundles":           {"managed_backups", 1, "sequence id destination source_instance_id manifest_sha256 created_at state"},
	"metadata":                         {"", 1, "key value"}, "projects": {"", 1, "id name github_repository_id repository"}, "workers": {"", 1, "project_id id name"}, "credentials": {"", 1, "id principal_id token_sha256 is_admin revoked"}, "grants": {"", 1, "credential_id project_id can_read can_write"}, "receipts": {"", 1, "principal_id project_id operation target key fingerprint response_json"}, "events": {"", 1, "sequence project_id event_type resource_id revision actor_id payload_json created_at"},
	"jobs": {"jobs", 1, "project_id id title brief worker_id status revision created_at updated_at"}, "job_history": {"jobs", 1, "project_id job_id revision actor_id state_json created_at"}, "job_checkpoints": {"jobs", 2, "sequence id project_id job_id job_revision actor_id worker_id done next blocker evidence_json created_at"},
	"inbox_conversations": {"inbox", 1, "project_id id root_message_id"}, "inbox_messages": {"inbox", 1, "sequence project_id id conversation_id sender_worker_id recipient_worker_id reply_to_message_id body created_at"}, "inbox_recipient_state": {"inbox", 1, "project_id message_id worker_id seen acknowledged revision"},
	"review_snapshots": {"reviews", 1, "project_id snapshot_id revision repository_id source observed_at recorded_at payload_sha256"}, "review_workspace": {"reviews", 1, "project_id revision snapshot_id"}, "review_pulls": {"reviews", 1, "project_id snapshot_id position number title state head_sha base_ref base_sha ci_state mergeability complete run_count attempt_count recent_runs_json latest_attempts_json"}, "review_runs": {"reviews", 1, "project_id snapshot_id pull_number run_order id source source_run_id channel head_sha reviewer scope outcome attributable complete started_at finished_at found accepted routed finalized fixed coverage_limits_json"}, "review_findings": {"reviews", 1, "project_id snapshot_id pull_number run_id position source_finding_id title detail severity disposition decision_reason actor decided_at fix_sha proof_note resolved_at"}, "review_attempts": {"reviews", 1, "project_id snapshot_id pull_number position source source_attempt_id channel head_sha state started_at finished_at duration_seconds source_run_id trigger_id provider_review_id"}, "review_retired_pulls": {"reviews", 1, "project_id snapshot_id position number reason"},
	"review_policy_config":    {"review_policy", 1, "project_id revision body_json actor_id created_at"},
	"review_execution_config": {"review_execution", 1, "project_id revision body_json"}, "review_operations": {"review_execution", 1, "id project_id actor_id worker_id configuration_revision pull_number channel head_sha state revision reason attempt_id created_at updated_at"}, "review_execution_attempts": {"review_execution", 1, "id operation_id project_id provider account state revision anchor_json intent external_id"}, "review_execution_reservations": {"review_execution", 1, "provider repository_id pull_number action_class account attempt_id"}, "review_execution_transitions": {"review_execution", 1, "attempt_id revision state reason external_id created_at"}, "review_cli_preflight": {"review_execution", 4, "attempt_id project_id body_json"}, "review_hosted_preflight": {"review_execution", 6, "attempt_id project_id body_json"}, "review_hosted_allowance_queries": {"review_execution", 6, "id project_id attempt_id operation_id capability_fingerprint revision body_json"},
	"review_execution_evidence": {"review_evidence", 1, "id project_id attempt_id operation_id body_json fingerprint"}, "review_hosted_observations": {"review_evidence", 3, "id project_id attempt_id operation_id position capture_sha256 capture_bytes body_json"},
	"review_adjudication_state": {"review_adjudication", 1, "evidence_id project_id revision body_json"}, "review_adjudication_changes": {"review_adjudication", 1, "evidence_id revision project_id actor_id kind finding_index input_json created_at"}, "review_finding_routes": {"review_adjudication", 2, "id project_id repository_id source_evidence_id finding_index source_pull_number source_head_sha target_pull_number state revision disposition has_fix created_at body_json"}, "review_finding_route_revisions": {"review_adjudication", 2, "route_id project_id revision actor_id kind created_at body_json"},
	"review_source_refreshes":        {"review_source_refresh", 1, "id project_id repository_id actor_id worker_id configuration_revision capability_id capability_fingerprint pull_number head_sha state revision reason created_at updated_at observation_json"},
	"review_checkpoint_publications": {"review_checkpoint_publication", 1, "id project_id evidence_id state revision created_at body_json"}, "review_checkpoint_revisions": {"review_checkpoint_publication", 1, "publication_id revision actor_id created_at body_json"}, "review_checkpoint_intents": {"review_checkpoint_publication", 1, "publication_id revision body_json"},
}

func validateSchema(ctx context.Context, db sqlReader, allowInactive bool) (map[string]int, error) {
	var version int
	if db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version) != nil || (version != 1 && version != 2) {
		return nil, invalid()
	}
	rows, err := db.QueryContext(ctx, `SELECT name FROM sqlite_schema WHERE type='table' AND name NOT LIKE 'sqlite_%' LIMIT 129`)
	if err != nil {
		return nil, invalid()
	}
	actual := map[string]bool{}
	for rows.Next() {
		var name string
		if rows.Scan(&name) != nil || len(actual) >= 128 {
			rows.Close()
			return nil, invalid()
		}
		actual[name] = true
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, invalid()
	}
	if !actual["metadata"] {
		return nil, invalid()
	}
	rows, err = db.QueryContext(ctx, `SELECT key,value FROM metadata WHERE key LIKE 'schema:%' OR key LIKE 'restore:%' LIMIT 129`)
	if err != nil {
		return nil, invalid()
	}
	versions := map[string]int{}
	count := 0
	for rows.Next() {
		count++
		var key, value string
		if rows.Scan(&key, &value) != nil || count > 128 {
			rows.Close()
			return nil, invalid()
		}
		if strings.HasPrefix(key, "restore:") {
			continue // Exact recovery state and history are validated below.
		}
		name := strings.TrimPrefix(key, "schema:")
		max, ok := supportedDomains[name]
		n, e := strconv.Atoi(value)
		if !ok || e != nil || n < 1 || n > max || strconv.Itoa(n) != value {
			rows.Close()
			return nil, invalid()
		}
		versions[name] = n
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, invalid()
	}
	if err = validateTriggers(ctx, db, versions["reviews"]); err != nil {
		return nil, err
	}
	for name := range actual {
		if _, ok := tables[name]; !ok {
			return nil, invalid()
		}
	}
	for name, spec := range tables {
		expected := spec.domain == "" || versions[spec.domain] >= spec.since
		if actual[name] != expected {
			return nil, invalid()
		}
		if !expected {
			continue
		}
		columns := spec.columns
		receiptPK := map[string]int{}
		if name == "receipts" {
			keys := []string{"principal_id", "project_id", "operation", "target", "key"}
			if version == 2 {
				columns = "instance_id " + columns
				keys = append([]string{"instance_id"}, keys...)
			}
			for i, key := range keys {
				receiptPK[key] = i + 1
			}
		}
		if name == "review_operations" {
			if versions[spec.domain] >= 2 {
				columns += " capability_fingerprint"
			}
			if versions[spec.domain] >= 3 {
				columns += " policy_revision"
			}
		}
		if name == "review_runs" && versions[spec.domain] >= 2 {
			columns += " snapshot_revision"
		}
		if name == "review_findings" && versions[spec.domain] >= 3 {
			columns += " route_json"
		}
		if name == "review_source_refreshes" {
			if versions[spec.domain] >= 2 {
				columns += " include_ci ci_observation_json"
			}
			if versions[spec.domain] >= 3 {
				columns += " include_readiness review_state_json"
			}
		}
		columnRows, e := db.QueryContext(ctx, `PRAGMA table_xinfo("`+name+`")`)
		if e != nil {
			return nil, invalid()
		}
		present := map[string]bool{}
		for columnRows.Next() {
			var cid, notnull, pk, hidden int
			var col, kind string
			var defaultValue any
			if columnRows.Scan(&cid, &col, &kind, &notnull, &defaultValue, &pk, &hidden) != nil || hidden != 0 {
				columnRows.Close()
				return nil, invalid()
			}
			if name == "receipts" && (kind != "TEXT" || notnull != 1 || defaultValue != nil || pk != receiptPK[col]) {
				columnRows.Close()
				return nil, invalid()
			}
			if name == "restore_history" && !recoveryHistoryColumn(col, kind, notnull, pk, hidden, defaultValue) {
				columnRows.Close()
				return nil, invalid()
			}
			present[col] = true
		}
		e = columnRows.Err()
		columnRows.Close()
		if e != nil {
			return nil, invalid()
		}
		required := strings.Fields(columns)
		if len(present) != len(required) {
			return nil, invalid()
		}
		for _, col := range required {
			if !present[col] {
				return nil, invalid()
			}
		}
		if err = validateForeignKeys(ctx, db, name); err != nil {
			return nil, err
		}
	}
	if version == 2 {
		var empty int
		err = db.QueryRowContext(ctx, `SELECT 1 FROM receipts WHERE instance_id='' LIMIT 1`).Scan(&empty)
		if !errors.Is(err, sql.ErrNoRows) {
			return nil, invalid()
		}
	}
	status, err := readRecoveryStatus(ctx, db)
	if err != nil || status.State == "inactive" && !allowInactive {
		return nil, invalid()
	}
	return versions, nil
}

// ValidateSource is a read-only preflight for operator commands before core.Open.
// Unlike immutable snapshots, a live source must read its current WAL.
func ValidateSource(ctx context.Context, dataDir string) error {
	if checkContext(ctx) != nil {
		return unavailable()
	}
	if strings.TrimSpace(dataDir) == "" || privateDirectory(dataDir, false) != nil {
		return invalid()
	}
	db, err := openDatabase(ctx, filepath.Join(dataDir, "firecontroller.sqlite"), false)
	if err != nil {
		return err
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return unavailable()
	}
	defer tx.Rollback()
	if _, err = validateSchema(ctx, tx, false); err != nil {
		return err
	}
	var identity, key string
	if tx.QueryRowContext(ctx, `SELECT value FROM metadata WHERE key='instance_id'`).Scan(&identity) != nil || !instanceID.MatchString(identity) || tx.QueryRowContext(ctx, `SELECT value FROM metadata WHERE key='signing_key'`).Scan(&key) != nil || !digest.MatchString(key) {
		return invalid()
	}
	return nil
}
func validateSnapshot(ctx context.Context, dataDir string, m Manifest) error {
	return validateSnapshotContent(ctx, dataDir, m, false)
}

// allowInactive is solely for Restore's post-transaction validation after its
// own exact inactive-marker/new-identity/revocation verification.
func validateSnapshotContent(ctx context.Context, dataDir string, m Manifest, allowInactive bool) error {
	db, err := openSnapshot(ctx, filepath.Join(dataDir, "firecontroller.sqlite"))
	if err != nil {
		return err
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return unavailable()
	}
	defer tx.Rollback()
	versions, err := validateSchema(ctx, tx, allowInactive)
	if err != nil {
		return err
	}
	// Published source bundles must leave room to archive their active source.
	// Live preflight/status/startup keep accepting a valid bounded history.
	if !allowInactive && versions["recovery_history"] != 0 {
		status, err := readRecoveryStatus(ctx, tx)
		var count int
		if err != nil || tx.QueryRowContext(ctx, `SELECT count(*) FROM restore_history`).Scan(&count) != nil || status.State == "coordination_active" && count >= maxRecoveryGenerations {
			return invalid()
		}
	}
	var identity, key string
	var sequence int64
	if tx.QueryRowContext(ctx, `SELECT value FROM metadata WHERE key='instance_id'`).Scan(&identity) != nil || identity != m.SourceInstanceID || !instanceID.MatchString(identity) || tx.QueryRowContext(ctx, `SELECT value FROM metadata WHERE key='signing_key'`).Scan(&key) != nil || !digest.MatchString(key) || tx.QueryRowContext(ctx, `SELECT coalesce(max(sequence),0) FROM events`).Scan(&sequence) != nil || sequence != m.EventSequence {
		return invalid()
	}
	integrity, err := tx.QueryContext(ctx, `PRAGMA integrity_check`)
	if err != nil {
		return invalid()
	}
	n := 0
	for integrity.Next() {
		var result string
		if integrity.Scan(&result) != nil || result != "ok" {
			integrity.Close()
			return invalid()
		}
		n++
	}
	err = integrity.Err()
	integrity.Close()
	if err != nil || n != 1 {
		return invalid()
	}
	foreign, err := tx.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		return invalid()
	}
	bad := foreign.Next()
	err = foreign.Err()
	foreign.Close()
	if bad || err != nil {
		return invalid()
	}
	refs := map[string]Artifact{}
	for _, a := range m.Artifacts {
		refs[a.Path] = a
	}
	if versions["review_evidence"] > 0 {
		if err = validateEvidenceReferences(ctx, tx, refs); err != nil {
			return err
		}
	}
	if versions["review_evidence"] >= 3 {
		if err = validateObservationReferences(ctx, tx, refs); err != nil {
			return err
		}
	}
	return checkContext(ctx)
}
func reference(refs map[string]Artifact, attempt, captureID, hash string, size int64, hosted bool) bool {
	if !attemptID.MatchString(attempt) || !digest.MatchString(hash) || captureID != "review_capture_"+hash[:32] || size <= 0 {
		return false
	}
	path := "review-captures/" + attempt + ".json"
	limit := maxCLICaptureBytes
	if hosted {
		path = "hosted-review-captures/" + attempt + "-hosted-" + hash + ".json"
		limit = maxHostedCaptureBytes
	}
	a, ok := refs[path]
	return ok && size <= limit && a.SHA256 == hash && a.Bytes == size
}
func validateEvidenceReferences(ctx context.Context, db sqlReader, refs map[string]Artifact) error {
	rows, err := db.QueryContext(ctx, `SELECT id,project_id,attempt_id,operation_id,CASE WHEN length(CAST(body_json AS BLOB))<=2105344 THEN body_json ELSE NULL END,fingerprint FROM review_execution_evidence`)
	if err != nil {
		return invalid()
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		count++
		if count > maxArtifacts {
			return invalid()
		}
		if checkContext(ctx) != nil {
			return unavailable()
		}
		var id, project, attempt, operation, fingerprint string
		var body sql.NullString
		if rows.Scan(&id, &project, &attempt, &operation, &body, &fingerprint) != nil || !body.Valid {
			return invalid()
		}
		var evidence reviewexec.ReviewEvidence
		if strictjson.Decode([]byte(body.String), &evidence) != nil {
			return invalid()
		}
		encoded, e := json.Marshal(evidence)
		if e != nil || string(encoded) != body.String || evidence.ID != id || evidence.ProjectID != project || evidence.AttemptID != attempt || evidence.OperationID != operation {
			return invalid()
		}
		capture := evidence.Capture
		owned, e := json.Marshal(capture)
		if e != nil {
			return invalid()
		}
		hash := sha256.Sum256(owned)
		if !digest.MatchString(fingerprint) || hex.EncodeToString(hash[:]) != fingerprint || !reference(refs, attempt, capture.CaptureID, capture.CaptureSHA256, capture.CaptureBytes, capture.Hosted != nil) {
			return invalid()
		}
	}
	if rows.Err() != nil {
		return invalid()
	}
	return nil
}
func validateObservationReferences(ctx context.Context, db sqlReader, refs map[string]Artifact) error {
	rows, err := db.QueryContext(ctx, `SELECT id,project_id,attempt_id,operation_id,position,capture_sha256,capture_bytes,CASE WHEN length(CAST(body_json AS BLOB))<=24576 THEN body_json ELSE NULL END FROM review_hosted_observations`)
	if err != nil {
		return invalid()
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		count++
		if count > maxArtifacts {
			return invalid()
		}
		if checkContext(ctx) != nil {
			return unavailable()
		}
		var id, project, attempt, operation, hash string
		var position, size int64
		var body sql.NullString
		if rows.Scan(&id, &project, &attempt, &operation, &position, &hash, &size, &body) != nil || !body.Valid {
			return invalid()
		}
		var observation reviewexec.HostedObservation
		if strictjson.Decode([]byte(body.String), &observation) != nil {
			return invalid()
		}
		encoded, e := json.Marshal(observation)
		if e != nil || string(encoded) != body.String || observation.ID != id || observation.ProjectID != project || observation.AttemptID != attempt || observation.OperationID != operation || observation.Position != position || position < 1 || position > 200 || observation.Input.CaptureSHA256 != hash || observation.Input.CaptureBytes != size || !reference(refs, attempt, observation.Input.CaptureID, hash, size, true) {
			return invalid()
		}
	}
	if rows.Err() != nil {
		return invalid()
	}
	return nil
}

// The only application triggers are defined by reviews schema version two.
// Exact text and table binding are required; inspection never executes custom
// trigger programs, and restore must reject them before rotating credentials.
func validateTriggers(ctx context.Context, db sqlReader, reviewVersion int) error {
	expected := map[string]string{}
	if reviewVersion >= 2 {
		expected["review_runs_snapshot_revision_insert"] = "CREATE TRIGGER review_runs_snapshot_revision_insert BEFORE INSERT ON review_runs WHEN NEW.snapshot_revision IS NOT (SELECT revision FROM review_snapshots WHERE project_id=NEW.project_id AND snapshot_id=NEW.snapshot_id) BEGIN SELECT RAISE(ABORT,'invalid review snapshot revision'); END"
		expected["review_runs_snapshot_revision_update"] = "CREATE TRIGGER review_runs_snapshot_revision_update BEFORE UPDATE OF project_id,snapshot_id,snapshot_revision ON review_runs WHEN NEW.snapshot_revision IS NOT (SELECT revision FROM review_snapshots WHERE project_id=NEW.project_id AND snapshot_id=NEW.snapshot_id) BEGIN SELECT RAISE(ABORT,'invalid review snapshot revision'); END"
	}
	rows, err := db.QueryContext(ctx, `SELECT name,tbl_name,CASE WHEN length(CAST(sql AS BLOB))<=2048 THEN sql ELSE NULL END FROM sqlite_schema WHERE type='trigger' LIMIT 3`)
	if err != nil {
		return invalid()
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		var name, table string
		var definition sql.NullString
		if rows.Scan(&name, &table, &definition) != nil || !definition.Valid {
			return invalid()
		}
		want, known := expected[name]
		if !known || table != "review_runs" || want != strings.TrimSpace(definition.String) {
			return invalid()
		}
		count++
	}
	if rows.Err() != nil || count != len(expected) {
		return invalid()
	}
	return nil
}

// Foreign keys are invariant across supported versions of each existing table;
// domain versions above select which tables must exist. Keep this compact list
// aligned with their owning core/jobs/inbox/reviews/reviewexec declarations.
// Grouped columns and every SQLite-exposed update/delete/match action matter:
// checking only existing rows cannot detect a cascade during identity rotation.
type foreignKey struct{ table, from, to, update, delete, match string }

func canonicalFK(table, from, to, onDelete string) foreignKey {
	return foreignKey{table, from, to, "NO ACTION", onDelete, "NONE"}
}

var canonicalForeignKeys = map[string][]foreignKey{
	"restore_history":                 {},
	"workers":                         {canonicalFK("projects", "project_id", "id", "NO ACTION")},
	"grants":                          {canonicalFK("credentials", "credential_id", "id", "NO ACTION"), canonicalFK("projects", "project_id", "id", "NO ACTION")},
	"events":                          {canonicalFK("projects", "project_id", "id", "NO ACTION")},
	"jobs":                            {canonicalFK("projects", "project_id", "id", "CASCADE"), canonicalFK("workers", "project_id,worker_id", "project_id,id", "NO ACTION")},
	"job_history":                     {canonicalFK("jobs", "project_id,job_id", "project_id,id", "CASCADE")},
	"job_checkpoints":                 {canonicalFK("jobs", "project_id,job_id", "project_id,id", "CASCADE")},
	"inbox_conversations":             {canonicalFK("projects", "project_id", "id", "NO ACTION"), canonicalFK("inbox_messages", "project_id,root_message_id", "project_id,id", "NO ACTION")},
	"inbox_messages":                  {canonicalFK("projects", "project_id", "id", "NO ACTION"), canonicalFK("inbox_conversations", "project_id,conversation_id", "project_id,id", "NO ACTION"), canonicalFK("workers", "project_id,sender_worker_id", "project_id,id", "NO ACTION"), canonicalFK("workers", "project_id,recipient_worker_id", "project_id,id", "NO ACTION"), canonicalFK("inbox_messages", "project_id,reply_to_message_id", "project_id,id", "NO ACTION")},
	"inbox_recipient_state":           {canonicalFK("inbox_messages", "project_id,message_id", "project_id,id", "NO ACTION"), canonicalFK("workers", "project_id,worker_id", "project_id,id", "NO ACTION")},
	"review_snapshots":                {canonicalFK("projects", "project_id", "id", "CASCADE")},
	"review_workspace":                {canonicalFK("projects", "project_id", "id", "CASCADE"), canonicalFK("review_snapshots", "project_id,snapshot_id", "project_id,snapshot_id", "NO ACTION")},
	"review_pulls":                    {canonicalFK("review_snapshots", "project_id,snapshot_id", "project_id,snapshot_id", "CASCADE")},
	"review_runs":                     {canonicalFK("review_pulls", "project_id,snapshot_id,pull_number", "project_id,snapshot_id,number", "CASCADE")},
	"review_findings":                 {canonicalFK("review_runs", "project_id,snapshot_id,pull_number,run_id", "project_id,snapshot_id,pull_number,id", "CASCADE")},
	"review_attempts":                 {canonicalFK("review_pulls", "project_id,snapshot_id,pull_number", "project_id,snapshot_id,number", "CASCADE")},
	"review_retired_pulls":            {canonicalFK("review_snapshots", "project_id,snapshot_id", "project_id,snapshot_id", "CASCADE")},
	"review_policy_config":            {canonicalFK("projects", "project_id", "id", "NO ACTION")},
	"review_execution_config":         {canonicalFK("projects", "project_id", "id", "NO ACTION")},
	"review_operations":               {canonicalFK("projects", "project_id", "id", "NO ACTION"), canonicalFK("review_execution_config", "project_id,configuration_revision", "project_id,revision", "NO ACTION")},
	"review_execution_attempts":       {canonicalFK("review_operations", "operation_id", "id", "NO ACTION"), canonicalFK("projects", "project_id", "id", "NO ACTION")},
	"review_execution_reservations":   {canonicalFK("review_execution_attempts", "attempt_id", "id", "NO ACTION")},
	"review_execution_transitions":    {canonicalFK("review_execution_attempts", "attempt_id", "id", "NO ACTION")},
	"review_cli_preflight":            {canonicalFK("review_execution_attempts", "attempt_id", "id", "NO ACTION"), canonicalFK("projects", "project_id", "id", "NO ACTION")},
	"review_hosted_preflight":         {canonicalFK("review_execution_attempts", "attempt_id", "id", "NO ACTION"), canonicalFK("projects", "project_id", "id", "NO ACTION")},
	"review_hosted_allowance_queries": {canonicalFK("projects", "project_id", "id", "NO ACTION"), canonicalFK("review_execution_attempts", "attempt_id", "id", "NO ACTION"), canonicalFK("review_operations", "operation_id", "id", "NO ACTION")},
	"review_execution_evidence":       {canonicalFK("projects", "project_id", "id", "NO ACTION"), canonicalFK("review_execution_attempts", "attempt_id", "id", "NO ACTION"), canonicalFK("review_operations", "operation_id", "id", "NO ACTION")},
	"review_hosted_observations":      {canonicalFK("projects", "project_id", "id", "NO ACTION"), canonicalFK("review_execution_attempts", "attempt_id", "id", "NO ACTION"), canonicalFK("review_operations", "operation_id", "id", "NO ACTION")},
	"review_adjudication_state":       {canonicalFK("review_execution_evidence", "evidence_id", "id", "NO ACTION"), canonicalFK("projects", "project_id", "id", "NO ACTION")},
	"review_adjudication_changes":     {canonicalFK("review_execution_evidence", "evidence_id", "id", "NO ACTION"), canonicalFK("projects", "project_id", "id", "NO ACTION")},
	"review_finding_routes":           {canonicalFK("projects", "project_id", "id", "NO ACTION"), canonicalFK("review_execution_evidence", "source_evidence_id", "id", "NO ACTION")},
	"review_finding_route_revisions":  {canonicalFK("review_finding_routes", "route_id", "id", "NO ACTION"), canonicalFK("projects", "project_id", "id", "NO ACTION")},
	"review_source_refreshes":         {canonicalFK("projects", "project_id", "id", "NO ACTION"), canonicalFK("review_execution_config", "project_id,configuration_revision", "project_id,revision", "NO ACTION")},
	"review_checkpoint_publications":  {canonicalFK("projects", "project_id", "id", "NO ACTION"), canonicalFK("review_execution_evidence", "evidence_id", "id", "NO ACTION")},
	"review_checkpoint_revisions":     {canonicalFK("review_checkpoint_publications", "publication_id", "id", "NO ACTION")},
	"review_checkpoint_intents":       {canonicalFK("review_checkpoint_publications", "publication_id", "id", "NO ACTION")},
}

func validateForeignKeys(ctx context.Context, db sqlReader, table string) error {
	rows, err := db.QueryContext(ctx, `PRAGMA foreign_key_list("`+table+`")`)
	if err != nil {
		return invalid()
	}
	defer rows.Close()
	type component struct{ parent, from, to, update, delete, match string }
	groups := map[int]map[int]component{}
	count := 0
	for rows.Next() {
		count++
		var id, seq int
		var parent, update, onDelete, match string
		var from, to sql.NullString
		if count > 64 || rows.Scan(&id, &seq, &parent, &from, &to, &update, &onDelete, &match) != nil || id < 0 || seq < 0 || seq >= 4 || !from.Valid || !to.Valid {
			return invalid()
		}
		if groups[id] == nil {
			if len(groups) >= 8 {
				return invalid()
			}
			groups[id] = map[int]component{}
		}
		if _, duplicate := groups[id][seq]; duplicate {
			return invalid()
		}
		groups[id][seq] = component{parent, from.String, to.String, update, onDelete, match}
	}
	if rows.Err() != nil {
		return invalid()
	}
	expected := map[foreignKey]int{}
	for _, key := range canonicalForeignKeys[table] {
		expected[key]++
	}
	for _, group := range groups {
		first, ok := group[0]
		if !ok {
			return invalid()
		}
		var from, to []string
		for i := 0; i < len(group); i++ {
			part, ok := group[i]
			if !ok || part.parent != first.parent || part.update != first.update || part.delete != first.delete || part.match != first.match {
				return invalid()
			}
			from = append(from, part.from)
			to = append(to, part.to)
		}
		key := foreignKey{first.parent, strings.Join(from, ","), strings.Join(to, ","), first.update, first.delete, first.match}
		if expected[key] < 1 {
			return invalid()
		}
		expected[key]--
	}
	for _, remaining := range expected {
		if remaining != 0 {
			return invalid()
		}
	}
	return nil
}
