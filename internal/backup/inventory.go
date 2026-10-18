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

	"github.com/benhook1013/FireController/internal/coordimport"
	"github.com/benhook1013/FireController/internal/historycollect"
	"github.com/benhook1013/FireController/internal/jobs"
	"github.com/benhook1013/FireController/internal/presentation"
	"github.com/benhook1013/FireController/internal/projectmap"
	"github.com/benhook1013/FireController/internal/publicsite"
	"github.com/benhook1013/FireController/internal/reviewexec"
	"github.com/benhook1013/FireController/internal/reviews"
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
				relative := name + "/" + entry.Name()
				_, limit, recognized := artifactPath(relative)
				if !recognized {
					continue
				}
				info, err := entry.Info()
				if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
					f.Close()
					return nil, invalid()
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
var supportedDomains = map[string]int{"coordination_import": 1, "history_collection": 1, "public_site": 1, "project_presentation": 1, "project_map": 2, "jobs": 6, "inbox": 1, "reviews": 5, "review_policy": 1, "review_execution": 6, "review_execution_indexes": 1, "review_evidence": 3, "review_adjudication": 2, "review_source_refresh": 3, "review_checkpoint_publication": 1, "review_comparison": 1, "review_route_policy": 1, "managed_backups": 1, "managed_backup_publications": 1, "recovery_history": 1}
var tables = map[string]tableVersion{
	"coordination_import_state":         {"coordination_import", 1, "project_id revision"},
	"coordination_import_records":       {"coordination_import", 1, "project_id source source_key job_id import_revision item_sha256 source_sha256 item_json created_at"},
	"coordination_import_batches":       {"coordination_import", 1, "project_id revision source source_sha256 payload_sha256 actor_id worker_id created_at"},
	"history_collections":               {"history_collection", 1, "id project_id repository_id actor_id worker_id reader_fingerprint selection_json state revision reason position_json pass_number page_count record_count created_at updated_at"},
	"history_collection_pages":          {"history_collection", 1, "id project_id collection_id sequence pass_number reader_fingerprint pull_number resource page_number record_count body_sha256 body_json created_at"},
	"history_collection_seen":           {"history_collection", 1, "project_id collection_id pass_number record_id page_id"},
	"review_route_policy_facts":         {"review_route_policy", 1, "route_id project_id target_pull_number revision active pending"},
	"review_route_policy_invalidations": {"review_route_policy", 1, "project_id target_pull_number route_id"},
	"review_comparison_proofs":          {"review_comparison", 1, "operation_id project_id pull_number channel state repository_id candidate_sha base_sha parent_sha parent_pull_number base_ref evidence_id"},
	"project_publications":              {"public_site", 1, "project_id revision slug enabled manifest_json round_salt generation public_revision public_updated_at refresh_error"},
	"public_project_snapshots":          {"public_site", 1, "project_id generation revision content_json created_at"},
	"managed_backup_publication_holds":  {"managed_backup_publications", 1, "bundle_id destination source_instance_id manifest_sha256 summary_json"},
	"project_presentations":             {"project_presentation", 1, "project_id revision state_json"},
	"project_presentation_history":      {"project_presentation", 1, "project_id revision actor_id state_json created_at"},
	"project_map_entries":               {"project_map", 1, "project_id id kind title state archived revision state_json"},
	"project_map_entry_history":         {"project_map", 1, "project_id entry_id revision actor_id state_json created_at"},
	"job_workstreams":                   {"project_map", 1, "project_id job_id revision workstream_id workstream_title"},
	"job_workstream_history":            {"project_map", 1, "project_id job_id revision workstream_id workstream_title actor_id job_revision created_at"},
	"project_map_import_sources":        {"project_map", 2, "project_id source_key revision record_json"},
	"project_map_import_history":        {"project_map", 2, "project_id source_key revision actor_id worker_id record_json created_at"},
	"project_map_import_items":          {"project_map", 2, "project_id source_key item_key entry_id entry_revision source_revision"},

	"notes":                      {"jobs", 6, "project_id id worker_id job_id phase kind body status status_reason revision created_at updated_at"},
	"note_revisions":             {"jobs", 6, "project_id note_id revision worker_id job_id phase kind body status status_reason created_at updated_at actor_id actor_worker_id recorded_at"},
	"worker_pause_state":         {"jobs", 5, "project_id worker_id revision paused reason"},
	"worker_pause_history":       {"jobs", 5, "project_id worker_id revision paused reason actor_id actor_worker_id created_at"},
	"worker_primary_jobs":        {"jobs", 4, "project_id worker_id revision job_id"},
	"worker_primary_job_history": {"jobs", 4, "project_id worker_id revision job_id actor_id actor_worker_id reason created_at"},
	"job_updates":                {"jobs", 3, "sequence id project_id job_id job_revision actor_id worker_id body evidence_json created_at"},
	"job_checklists":             {"jobs", 3, "project_id job_id revision job_revision actor_id worker_id items_json created_at"},
	"job_checklist_history":      {"jobs", 3, "project_id job_id revision job_revision actor_id worker_id items_json created_at"},
	"restore_history":            {"recovery_history", 1, "instance_id body_json"},
	"managed_backup_state":       {"managed_backups", 1, "singleton body_json"},
	"managed_backup_bundles":     {"managed_backups", 1, "sequence id destination source_instance_id manifest_sha256 created_at state"},
	"metadata":                   {"", 1, "key value"}, "projects": {"", 1, "id name github_repository_id repository"}, "workers": {"", 1, "project_id id name"}, "credentials": {"", 1, "id principal_id token_sha256 is_admin revoked"}, "grants": {"", 1, "credential_id project_id can_read can_write"}, "receipts": {"", 1, "principal_id project_id operation target key fingerprint response_json"}, "events": {"", 1, "sequence project_id event_type resource_id revision actor_id payload_json created_at"},
	"jobs": {"jobs", 1, "project_id id title brief worker_id status revision created_at updated_at"}, "job_history": {"jobs", 1, "project_id job_id revision actor_id state_json created_at"}, "job_checkpoints": {"jobs", 2, "sequence id project_id job_id job_revision actor_id worker_id done next blocker evidence_json created_at"},
	"inbox_conversations": {"inbox", 1, "project_id id root_message_id"}, "inbox_messages": {"inbox", 1, "sequence project_id id conversation_id sender_worker_id recipient_worker_id reply_to_message_id body created_at"}, "inbox_recipient_state": {"inbox", 1, "project_id message_id worker_id seen acknowledged revision"},
	"review_snapshots": {"reviews", 1, "project_id snapshot_id revision repository_id source observed_at recorded_at payload_sha256"}, "review_workspace": {"reviews", 1, "project_id revision snapshot_id"}, "review_pulls": {"reviews", 1, "project_id snapshot_id position number title state head_sha base_ref base_sha ci_state mergeability complete run_count attempt_count recent_runs_json latest_attempts_json"}, "review_runs": {"reviews", 1, "project_id snapshot_id pull_number run_order id source source_run_id channel head_sha reviewer scope outcome attributable complete started_at finished_at found accepted routed finalized fixed coverage_limits_json"}, "review_findings": {"reviews", 1, "project_id snapshot_id pull_number run_id position source_finding_id title detail severity disposition decision_reason actor decided_at fix_sha proof_note resolved_at"}, "review_attempts": {"reviews", 1, "project_id snapshot_id pull_number position source source_attempt_id channel head_sha state started_at finished_at duration_seconds source_run_id trigger_id provider_review_id"}, "review_retired_pulls": {"reviews", 1, "project_id snapshot_id position number reason"},
	"review_pull_versions":         {"reviews", 4, "project_id number version_revision title state head_sha base_ref base_sha ci_state mergeability complete run_count attempt_count recent_runs_json latest_attempts_json"},
	"review_run_versions":          {"reviews", 4, "project_id id version_revision pull_number source source_run_id channel head_sha reviewer scope outcome attributable complete started_at finished_at found accepted routed finalized fixed coverage_limits_json"},
	"review_finding_versions":      {"reviews", 4, "project_id run_id run_version_revision pull_number position source_finding_id title detail severity disposition decision_reason actor decided_at fix_sha proof_note resolved_at route_json"},
	"review_attempt_versions":      {"reviews", 4, "project_id source source_attempt_id version_revision pull_number channel head_sha state started_at finished_at duration_seconds source_run_id trigger_id provider_review_id"},
	"review_retired_versions":      {"reviews", 4, "project_id number version_revision reason"},
	"review_pull_memberships":      {"reviews", 4, "project_id number start_revision end_revision version_revision position"},
	"review_run_memberships":       {"reviews", 4, "project_id id start_revision end_revision version_revision pull_number run_order"},
	"review_attempt_memberships":   {"reviews", 4, "project_id source source_attempt_id start_revision end_revision version_revision pull_number position"},
	"review_retired_memberships":   {"reviews", 4, "project_id number start_revision end_revision version_revision position"},
	"review_current_pulls":         {"reviews", 4, "project_id number start_revision position"},
	"review_current_runs":          {"reviews", 4, "project_id id start_revision pull_number run_order"},
	"review_current_attempts":      {"reviews", 4, "project_id source source_attempt_id start_revision pull_number position"},
	"review_current_retired":       {"reviews", 4, "project_id number start_revision position"},
	"review_current_totals":        {"reviews", 5, "project_id pull_number run_count attempt_count incomplete_run_count"},
	"review_current_attempt_order": {"reviews", 5, "project_id source source_attempt_id pull_number channel sort_key position"},
	"review_policy_config":         {"review_policy", 1, "project_id revision body_json actor_id created_at"},
	"review_execution_config":      {"review_execution", 1, "project_id revision body_json"}, "review_operations": {"review_execution", 1, "id project_id actor_id worker_id configuration_revision pull_number channel head_sha state revision reason attempt_id created_at updated_at"}, "review_execution_attempts": {"review_execution", 1, "id operation_id project_id provider account state revision anchor_json intent external_id"}, "review_execution_reservations": {"review_execution", 1, "provider repository_id pull_number action_class account attempt_id"}, "review_execution_transitions": {"review_execution", 1, "attempt_id revision state reason external_id created_at"}, "review_cli_preflight": {"review_execution", 4, "attempt_id project_id body_json"}, "review_hosted_preflight": {"review_execution", 6, "attempt_id project_id body_json"}, "review_hosted_allowance_queries": {"review_execution", 6, "id project_id attempt_id operation_id capability_fingerprint revision body_json"},
	"review_execution_evidence": {"review_evidence", 1, "id project_id attempt_id operation_id body_json fingerprint"}, "review_hosted_observations": {"review_evidence", 3, "id project_id attempt_id operation_id position capture_sha256 capture_bytes body_json"},
	"review_adjudication_state": {"review_adjudication", 1, "evidence_id project_id revision body_json"}, "review_adjudication_changes": {"review_adjudication", 1, "evidence_id revision project_id actor_id kind finding_index input_json created_at"}, "review_finding_routes": {"review_adjudication", 2, "id project_id repository_id source_evidence_id finding_index source_pull_number source_head_sha target_pull_number state revision disposition has_fix created_at body_json"}, "review_finding_route_revisions": {"review_adjudication", 2, "route_id project_id revision actor_id kind created_at body_json"},
	"review_source_refreshes":        {"review_source_refresh", 1, "id project_id repository_id actor_id worker_id configuration_revision capability_id capability_fingerprint pull_number head_sha state revision reason created_at updated_at observation_json"},
	"review_checkpoint_publications": {"review_checkpoint_publication", 1, "id project_id evidence_id state revision created_at body_json"}, "review_checkpoint_revisions": {"review_checkpoint_publication", 1, "publication_id revision actor_id created_at body_json"}, "review_checkpoint_intents": {"review_checkpoint_publication", 1, "publication_id revision body_json"},
}

// workerStateColumn preserves the composite identities and explicit values
// required by worker-state UPSERTs and immutable per-worker revision history.
func workerStateColumn(table, column, kind string, notnull, pk int, defaultValue any) bool {
	if defaultValue != nil {
		return false
	}
	history := table == "worker_primary_job_history" || table == "worker_pause_history"
	pause := table == "worker_pause_state" || table == "worker_pause_history"
	expectedKind, expectedNotNull, expectedPK := "TEXT", 1, 0
	switch column {
	case "project_id":
		expectedPK = 1
	case "worker_id":
		expectedPK = 2
	case "revision":
		expectedKind = "INTEGER"
		if history {
			expectedPK = 3
		}
	case "job_id":
		if pause {
			return false
		}
		expectedNotNull = 0
	case "paused":
		if !pause {
			return false
		}
		expectedKind = "INTEGER"
	case "reason":
		if !history && !pause {
			return false
		}
	case "actor_id", "actor_worker_id", "created_at":
		if !history {
			return false
		}
	default:
		return false
	}
	return kind == expectedKind && notnull == expectedNotNull && pk == expectedPK
}

func noteColumn(table, column, kind string, notnull, pk int, defaultValue any) bool {
	if defaultValue != nil {
		return false
	}
	expectedKind, expectedNull, expectedPK := "TEXT", 1, 0
	switch column {
	case "project_id":
		expectedPK = 1
	case "id", "note_id":
		expectedPK = 2
	case "revision":
		expectedKind = "INTEGER"
		if table == "note_revisions" {
			expectedPK = 3
		}
	case "worker_id", "job_id":
		expectedNull = 0
	case "phase", "kind", "body", "status", "status_reason", "created_at", "updated_at", "actor_id", "actor_worker_id", "recorded_at":
	default:
		return false
	}
	return kind == expectedKind && notnull == expectedNull && pk == expectedPK
}

// Map UPSERTs and retained identities depend on exact integer/nullable and key shapes.
func projectMapColumn(table, column, kind string, notnull, pk int, defaultValue any) bool {
	if defaultValue != nil {
		return false
	}
	expectedKind, expectedNull, expectedPK := "TEXT", 1, 0
	switch column {
	case "project_id":
		expectedPK = 1
	case "id", "entry_id", "job_id":
		if table != "project_map_import_items" {
			expectedPK = 2
		}
	case "source_key":
		expectedPK = 2
	case "item_key":
		expectedPK = 3
	case "revision":
		expectedKind = "INTEGER"
		if table == "project_map_entry_history" || table == "job_workstream_history" || table == "project_map_import_history" {
			expectedPK = 3
		}
	case "archived", "job_revision", "entry_revision", "source_revision":
		expectedKind = "INTEGER"
	case "workstream_id":
		expectedNull = 0
	case "kind", "title", "state", "state_json", "actor_id", "worker_id", "record_json", "created_at", "workstream_title":
	default:
		return false
	}
	return kind == expectedKind && notnull == expectedNull && pk == expectedPK
}

func presentationColumn(table, column, kind string, notnull, pk int, defaultValue any) bool {
	if defaultValue != nil {
		return false
	}
	expectedKind, expectedPK := "TEXT", 0
	switch column {
	case "project_id":
		expectedPK = 1
	case "revision":
		expectedKind = "INTEGER"
		if table == "project_presentation_history" {
			expectedPK = 2
		}
	case "state_json", "actor_id", "created_at":
	default:
		return false
	}
	return kind == expectedKind && notnull == 1 && pk == expectedPK
}

// Public publication UPSERTs and generation-scoped immutable snapshots depend
// on these exact identities, types and explicit values, not just column names.
func publicSiteColumn(table, column, kind string, notnull, pk int, defaultValue any) bool {
	if defaultValue != nil {
		return false
	}
	expectedKind, expectedPK := "TEXT", 0
	switch column {
	case "project_id":
		expectedPK = 1
	case "generation":
		if table == "public_project_snapshots" {
			expectedPK = 2
		}
	case "revision":
		expectedKind = "INTEGER"
		if table == "public_project_snapshots" {
			expectedPK = 3
		}
	case "enabled", "public_revision":
		expectedKind = "INTEGER"
	case "slug", "manifest_json", "round_salt", "public_updated_at", "refresh_error", "content_json", "created_at":
	default:
		return false
	}
	return kind == expectedKind && notnull == 1 && pk == expectedPK
}

func validatePublicSiteIdentity(ctx context.Context, db sqlReader) error {
	var uniqueSlug bool
	if db.QueryRowContext(ctx, `SELECT EXISTS (
 SELECT 1 FROM pragma_index_list('project_publications') i WHERE i."unique"=1 AND i.partial=0
 AND (SELECT count(*) FROM pragma_index_xinfo(i.name) WHERE key=1)=1
 AND EXISTS (SELECT 1 FROM pragma_index_xinfo(i.name) WHERE key=1 AND name='slug' AND cid>=0 AND coll='BINARY')
)`).Scan(&uniqueSlug) != nil || !uniqueSlug {
		return invalid()
	}
	return nil
}

// These identities are required by checklist UPSERTs, immutable revisions and
// server-generated update sequences. Column names alone do not establish them.
func jobProgressColumn(table, column, kind string, notnull, pk int, defaultValue any) bool {
	if defaultValue != nil {
		return false
	}
	expectedKind, expectedNull, expectedPK := "TEXT", 1, 0
	updates := table == "job_updates"
	switch column {
	case "sequence":
		if !updates {
			return false
		}
		expectedKind, expectedNull, expectedPK = "INTEGER", 0, 1
	case "project_id":
		if !updates {
			expectedPK = 1
		}
	case "job_id":
		if !updates {
			expectedPK = 2
		}
	case "revision":
		if updates {
			return false
		}
		expectedKind = "INTEGER"
		if table == "job_checklist_history" {
			expectedPK = 3
		}
	case "job_revision":
		expectedKind = "INTEGER"
	case "id", "body", "evidence_json":
		if !updates {
			return false
		}
	case "items_json":
		if updates {
			return false
		}
	case "actor_id", "worker_id", "created_at":
	default:
		return false
	}
	return kind == expectedKind && notnull == expectedNull && pk == expectedPK
}

// SQLite exposes the primary-key shape through table_xinfo but not whether its
// generated rowid is AUTOINCREMENT. Verify the supported migration's leading
// declaration, rather than matching that keyword inside comments or strings.
var jobUpdateSequence = regexp.MustCompile(`(?i)^CREATE\s+TABLE\s+(?:job_updates|"job_updates")\s*\(\s*sequence\s+INTEGER\s+PRIMARY\s+KEY\s+AUTOINCREMENT\s*,`)

func validateJobUpdateIdentity(ctx context.Context, db sqlReader) error {
	var definition sql.NullString
	if db.QueryRowContext(ctx, `SELECT CASE WHEN length(CAST(sql AS BLOB))<=4096 THEN sql ELSE NULL END FROM sqlite_schema WHERE type='table' AND name='job_updates'`).Scan(&definition) != nil || !definition.Valid || !jobUpdateSequence.MatchString(definition.String) {
		return invalid()
	}
	// The generated opaque ID is independently unique; an index with extra
	// columns, an expression, a partial predicate or another collation cannot
	// substitute for that identity. Index names are not part of the contract.
	var uniqueID bool
	if db.QueryRowContext(ctx, `SELECT EXISTS (
 SELECT 1 FROM pragma_index_list('job_updates') i WHERE i."unique"=1 AND i.partial=0
 AND (SELECT count(*) FROM pragma_index_xinfo(i.name) WHERE key=1)=1
 AND EXISTS (SELECT 1 FROM pragma_index_xinfo(i.name) WHERE key=1 AND name='id' AND cid>=0 AND coll='BINARY')
)`).Scan(&uniqueID) != nil || !uniqueID {
		return invalid()
	}
	return nil
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
	rows, err = db.QueryContext(ctx, `SELECT CASE WHEN length(CAST(key AS BLOB))<=128 THEN key END,CASE WHEN length(CAST(value AS BLOB))<=4096 THEN value END FROM metadata WHERE key LIKE 'schema:%' OR key LIKE 'restore:%' LIMIT 129`)
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
	if err = validateTriggers(ctx, db, versions["reviews"], versions["review_comparison"], versions["review_route_policy"]); err != nil {
		return nil, err
	}
	if err = validateHistoryViews(ctx, db, versions["reviews"]); err != nil {
		return nil, err
	}
	if versions["reviews"] >= 4 {
		if err = validateHistoryPhysicalSchema(ctx, db, versions["reviews"]); err != nil {
			return nil, err
		}
	}
	if versions["review_comparison"] > 0 {
		if err = validateComparisonPhysicalSchema(ctx, db); err != nil {
			return nil, err
		}
	}
	if versions["review_route_policy"] > 0 {
		if err = validateRoutePolicyPhysicalSchema(ctx, db); err != nil {
			return nil, err
		}
	}
	if versions["history_collection"] > 0 {
		if err = validateHistoryCollectionPhysicalSchema(ctx, db); err != nil {
			return nil, err
		}
	}
	for name := range actual {
		if _, ok := tables[name]; !ok {
			return nil, invalid()
		}
	}
	for name, spec := range tables {
		expected := spec.domain == "" || versions[spec.domain] >= spec.since
		if spec.domain == "reviews" && versions["reviews"] >= 4 {
			switch name {
			case "review_pulls", "review_runs", "review_findings", "review_attempts", "review_retired_pulls":
				expected = false // Version four retains these as canonical read views.
			}
		}
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
			if (name == "worker_primary_jobs" || name == "worker_primary_job_history" || name == "worker_pause_state" || name == "worker_pause_history") && !workerStateColumn(name, col, kind, notnull, pk, defaultValue) {
				columnRows.Close()
				return nil, invalid()
			}
			if (name == "notes" || name == "note_revisions") && !noteColumn(name, col, kind, notnull, pk, defaultValue) {
				columnRows.Close()
				return nil, invalid()
			}
			if spec.domain == "project_map" && !projectMapColumn(name, col, kind, notnull, pk, defaultValue) {
				columnRows.Close()
				return nil, invalid()
			}
			if spec.domain == "project_presentation" && !presentationColumn(name, col, kind, notnull, pk, defaultValue) {
				columnRows.Close()
				return nil, invalid()
			}
			if spec.domain == "public_site" && !publicSiteColumn(name, col, kind, notnull, pk, defaultValue) {
				columnRows.Close()
				return nil, invalid()
			}
			if (name == "job_updates" || name == "job_checklists" || name == "job_checklist_history") && !jobProgressColumn(name, col, kind, notnull, pk, defaultValue) {
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
		if name == "project_publications" {
			if err = validatePublicSiteIdentity(ctx, db); err != nil {
				return nil, err
			}
		}
		if name == "job_updates" {
			if err = validateJobUpdateIdentity(ctx, db); err != nil {
				return nil, err
			}
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
	if err := CheckDataDirectory(dataDir); err != nil {
		return err
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
	if versions["coordination_import"] > 0 {
		if err := coordimport.ValidateSnapshot(ctx, tx); err != nil {
			if checkContext(ctx) != nil {
				return unavailable()
			}
			return invalid()
		}
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
	if versions["reviews"] >= 4 {
		if err = reviews.ValidateHistoryReferences(ctx, tx); err != nil {
			if checkContext(ctx) != nil {
				return unavailable()
			}
			return invalid()
		}
	}
	if versions["reviews"] >= 5 {
		if err = reviews.ValidateCurrentSummaries(ctx, tx); err != nil {
			if checkContext(ctx) != nil {
				return unavailable()
			}
			return invalid()
		}
	}
	if versions["jobs"] >= 6 {
		if err = jobs.ValidateNotesSnapshot(ctx, tx); err != nil {
			if checkContext(ctx) != nil {
				return unavailable()
			}
			return invalid()
		}
	}
	if versions["project_map"] > 0 {
		if err = projectmap.ValidateSnapshot(ctx, tx); err != nil {
			if checkContext(ctx) != nil {
				return unavailable()
			}
			return invalid()
		}
	}
	if versions["project_presentation"] > 0 {
		if err = presentation.ValidateSnapshot(ctx, tx); err != nil {
			if checkContext(ctx) != nil {
				return unavailable()
			}
			return invalid()
		}
	}
	if versions["public_site"] > 0 {
		if err = publicsite.ValidateSnapshot(ctx, tx); err != nil {
			if checkContext(ctx) != nil {
				return unavailable()
			}
			return invalid()
		}
	}
	if versions["history_collection"] > 0 {
		if err = historycollect.ValidateSnapshot(ctx, tx); err != nil {
			if checkContext(ctx) != nil {
				return unavailable()
			}
			return invalid()
		}
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
	if versions["review_comparison"] > 0 {
		if err = reviewexec.ValidateComparisonProofs(ctx, tx); err != nil {
			if checkContext(ctx) != nil {
				return unavailable()
			}
			return invalid()
		}
	}
	if versions["review_route_policy"] > 0 {
		if err = reviewexec.ValidateRoutePolicyFacts(ctx, tx); err != nil {
			if checkContext(ctx) != nil {
				return unavailable()
			}
			return invalid()
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

// Stored DDL omits the final statement delimiter. Otherwise canonical
// definitions must match exactly; inspection never executes an incoming view.
func historyDefinitionSQL(definition string) string {
	return strings.TrimSuffix(strings.TrimSpace(definition), ";")
}

func validateHistoryViews(ctx context.Context, db sqlReader, reviewVersion int) error {
	expected := map[string]string{}
	if reviewVersion >= 4 {
		expected = reviews.HistoryReadViewSQL()
	}
	rows, err := db.QueryContext(ctx, `SELECT CASE WHEN length(CAST(name AS BLOB))<=128 THEN name ELSE NULL END,CASE WHEN length(CAST(sql AS BLOB))<=16384 THEN sql ELSE NULL END FROM sqlite_schema WHERE type='view' LIMIT 6`)
	if err != nil {
		return invalid()
	}
	defer rows.Close()
	seen := map[string]bool{}
	for rows.Next() {
		var name string
		var definition sql.NullString
		if rows.Scan(&name, &definition) != nil || !definition.Valid || seen[name] {
			return invalid()
		}
		want, known := expected[name]
		if !known || historyDefinitionSQL(definition.String) != historyDefinitionSQL(want) {
			return invalid()
		}
		seen[name] = true
	}
	if rows.Err() != nil || len(seen) != len(expected) {
		return invalid()
	}
	return nil
}

// Exact canonical DDL binds CHECK, PK, UNIQUE, default and partial-index
// semantics in addition to the independent column/FK inventory below.
func validateHistoryPhysicalSchema(ctx context.Context, db sqlReader, version int) error {
	type object struct{ kind, definition string }
	expected := map[string]object{}
	var names []string
	definitions := reviews.HistorySchemaSQL()
	expectedTables, expectedObjects := 13, 22
	if version >= 5 {
		definitions = append(definitions, reviews.CurrentSummarySchemaSQL()...)
		expectedTables, expectedObjects = 15, 25
	}
	for _, definition := range definitions {
		fields := strings.Fields(definition)
		if len(fields) < 4 || fields[0] != "CREATE" {
			return invalid()
		}
		kind, name := "table", fields[2]
		switch fields[1] {
		case "TABLE":
			names = append(names, name)
		case "INDEX":
			kind = "index"
		case "UNIQUE":
			if len(fields) < 5 || fields[2] != "INDEX" {
				return invalid()
			}
			kind, name = "index", fields[3]
		default:
			return invalid()
		}
		if _, duplicate := expected[name]; duplicate {
			return invalid()
		}
		expected[name] = object{kind, historyDefinitionSQL(definition)}
	}
	if len(names) != expectedTables || len(expected) != expectedObjects {
		return invalid()
	}
	placeholders := make([]string, len(names))
	args := make([]any, len(names))
	for i, name := range names {
		placeholders[i] = "?"
		args[i] = name
	}
	rows, err := db.QueryContext(ctx, `SELECT type,CASE WHEN length(CAST(name AS BLOB))<=128 THEN name ELSE NULL END,CASE WHEN length(CAST(sql AS BLOB))<=16384 THEN sql ELSE NULL END FROM sqlite_schema WHERE (type='table' OR (type='index' AND sql IS NOT NULL)) AND tbl_name IN (`+strings.Join(placeholders, ",")+`) LIMIT `+strconv.Itoa(expectedObjects+1), args...)
	if err != nil {
		return invalid()
	}
	defer rows.Close()
	seen := map[string]bool{}
	for rows.Next() {
		var kind, name string
		var definition sql.NullString
		if rows.Scan(&kind, &name, &definition) != nil || !definition.Valid || seen[name] {
			return invalid()
		}
		want, known := expected[name]
		if !known || kind != want.kind || historyDefinitionSQL(definition.String) != want.definition {
			return invalid()
		}
		seen[name] = true
	}
	if rows.Err() != nil || len(seen) != len(expected) {
		return invalid()
	}
	return nil
}

// Reviews versions two and three contain exact revision triggers; comparison
// and route-policy domains additionally own exact invalidation/retention triggers.
// Exact text and table binding are required; inspection never executes custom
// trigger programs, and restore must reject them before rotating credentials.
func validateTriggers(ctx context.Context, db sqlReader, reviewVersion, comparisonVersion, routePolicyVersion int) error {
	expected := map[string]string{}
	expectedTables := map[string]string{}
	if reviewVersion >= 2 && reviewVersion <= 3 {
		expected["review_runs_snapshot_revision_insert"] = "CREATE TRIGGER review_runs_snapshot_revision_insert BEFORE INSERT ON review_runs WHEN NEW.snapshot_revision IS NOT (SELECT revision FROM review_snapshots WHERE project_id=NEW.project_id AND snapshot_id=NEW.snapshot_id) BEGIN SELECT RAISE(ABORT,'invalid review snapshot revision'); END"
		expected["review_runs_snapshot_revision_update"] = "CREATE TRIGGER review_runs_snapshot_revision_update BEFORE UPDATE OF project_id,snapshot_id,snapshot_revision ON review_runs WHEN NEW.snapshot_revision IS NOT (SELECT revision FROM review_snapshots WHERE project_id=NEW.project_id AND snapshot_id=NEW.snapshot_id) BEGIN SELECT RAISE(ABORT,'invalid review snapshot revision'); END"
		for name := range expected {
			expectedTables[name] = "review_runs"
		}
	}
	var nativeDefinitions []string
	if comparisonVersion > 0 {
		nativeDefinitions = append(nativeDefinitions, reviewexec.ComparisonSchemaSQL()...)
	}
	if routePolicyVersion > 0 {
		nativeDefinitions = append(nativeDefinitions, reviewexec.RoutePolicySchemaSQL()...)
	}
	for _, statement := range nativeDefinitions {
		fields := strings.Fields(statement)
		if len(fields) < 4 || fields[1] != "TRIGGER" {
			continue
		}
		expected[fields[2]] = historyDefinitionSQL(statement)
		for i, field := range fields {
			if field == "ON" && i+1 < len(fields) {
				expectedTables[fields[2]] = fields[i+1]
				break
			}
		}
	}
	rows, err := db.QueryContext(ctx, `SELECT CASE WHEN length(CAST(name AS BLOB))<=128 THEN name ELSE NULL END,tbl_name,CASE WHEN length(CAST(sql AS BLOB))<=16384 THEN sql ELSE NULL END FROM sqlite_schema WHERE type='trigger' LIMIT `+strconv.Itoa(len(expected)+1))
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
		if !known || table != expectedTables[name] || want != historyDefinitionSQL(definition.String) {
			return invalid()
		}
		count++
	}
	if rows.Err() != nil || count != len(expected) {
		return invalid()
	}
	return nil
}

// Bind every comparison table/index detail to the frozen owning DDL, including
// primary keys, defaults and both scoped comparison indexes.
func validateComparisonPhysicalSchema(ctx context.Context, db sqlReader) error {
	type object struct{ kind, definition string }
	expected := map[string]object{}
	for _, statement := range reviewexec.ComparisonSchemaSQL() {
		fields := strings.Fields(statement)
		if len(fields) < 4 {
			return invalid()
		}
		if fields[1] == "TABLE" || fields[1] == "INDEX" {
			expected[fields[2]] = object{strings.ToLower(fields[1]), historyDefinitionSQL(statement)}
		}
	}
	if len(expected) != 3 {
		return invalid()
	}
	rows, err := db.QueryContext(ctx, `SELECT type,CASE WHEN length(CAST(name AS BLOB))<=128 THEN name ELSE NULL END,CASE WHEN length(CAST(sql AS BLOB))<=16384 THEN sql ELSE NULL END FROM sqlite_schema WHERE tbl_name='review_comparison_proofs' AND (type='table' OR (type='index' AND sql IS NOT NULL)) LIMIT 4`)
	if err != nil {
		return invalid()
	}
	defer rows.Close()
	seen := map[string]bool{}
	for rows.Next() {
		var kind, name string
		var definition sql.NullString
		if rows.Scan(&kind, &name, &definition) != nil || !definition.Valid || seen[name] {
			return invalid()
		}
		want, known := expected[name]
		if !known || kind != want.kind || historyDefinitionSQL(definition.String) != want.definition {
			return invalid()
		}
		seen[name] = true
	}
	if rows.Err() != nil || len(seen) != len(expected) {
		return invalid()
	}
	return nil
}

// These derived tables are optional for supported predecessors. When present,
// every table/index detail must match their owning canonical definitions.
func validateRoutePolicyPhysicalSchema(ctx context.Context, db sqlReader) error {
	type object struct{ kind, definition string }
	expected := map[string]object{}
	for _, statement := range reviewexec.RoutePolicySchemaSQL() {
		fields := strings.Fields(statement)
		if len(fields) < 4 {
			return invalid()
		}
		if fields[1] == "TABLE" || fields[1] == "INDEX" {
			expected[fields[2]] = object{strings.ToLower(fields[1]), historyDefinitionSQL(statement)}
		}
	}
	if len(expected) != 4 {
		return invalid()
	}
	rows, err := db.QueryContext(ctx, `SELECT type,CASE WHEN length(CAST(name AS BLOB))<=128 THEN name ELSE NULL END,CASE WHEN length(CAST(sql AS BLOB))<=16384 THEN sql ELSE NULL END FROM sqlite_schema WHERE tbl_name IN ('review_route_policy_facts','review_route_policy_invalidations') AND (type='table' OR (type='index' AND sql IS NOT NULL)) LIMIT 5`)
	if err != nil {
		return invalid()
	}
	defer rows.Close()
	seen := map[string]bool{}
	for rows.Next() {
		var kind, name string
		var definition sql.NullString
		if rows.Scan(&kind, &name, &definition) != nil || !definition.Valid || seen[name] {
			return invalid()
		}
		want, known := expected[name]
		if !known || kind != want.kind || historyDefinitionSQL(definition.String) != want.definition {
			return invalid()
		}
		seen[name] = true
	}
	if rows.Err() != nil || len(seen) != len(expected) {
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
	"coordination_import_state":         {canonicalFK("projects", "project_id", "id", "NO ACTION")},
	"coordination_import_records":       {canonicalFK("jobs", "project_id,job_id", "project_id,id", "NO ACTION"), canonicalFK("coordination_import_batches", "project_id,import_revision", "project_id,revision", "NO ACTION")},
	"coordination_import_batches":       {canonicalFK("projects", "project_id", "id", "NO ACTION")},
	"history_collections":               {canonicalFK("projects", "project_id", "id", "NO ACTION")},
	"history_collection_pages":          {canonicalFK("history_collections", "project_id,collection_id", "project_id,id", "NO ACTION")},
	"history_collection_seen":           {canonicalFK("history_collection_pages", "project_id,collection_id,page_id", "project_id,collection_id,id", "NO ACTION")},
	"review_route_policy_facts":         {canonicalFK("review_finding_routes", "route_id", "id", "NO ACTION"), canonicalFK("projects", "project_id", "id", "NO ACTION")},
	"review_route_policy_invalidations": {canonicalFK("projects", "project_id", "id", "NO ACTION")},
	"review_comparison_proofs":          {canonicalFK("review_operations", "operation_id", "id", "NO ACTION"), canonicalFK("projects", "project_id", "id", "NO ACTION")},
	"project_publications":              {canonicalFK("projects", "project_id", "id", "CASCADE")},
	"public_project_snapshots":          {canonicalFK("project_publications", "project_id", "project_id", "CASCADE")},
	"project_presentations":             {canonicalFK("projects", "project_id", "id", "CASCADE")},
	"project_presentation_history":      {canonicalFK("project_presentations", "project_id", "project_id", "CASCADE")},
	"project_map_entries":               {canonicalFK("projects", "project_id", "id", "CASCADE")},
	"project_map_entry_history":         {canonicalFK("project_map_entries", "project_id,entry_id", "project_id,id", "CASCADE")},
	"job_workstreams":                   {canonicalFK("jobs", "project_id,job_id", "project_id,id", "CASCADE"), canonicalFK("project_map_entries", "project_id,workstream_id", "project_id,id", "NO ACTION")},
	"job_workstream_history":            {canonicalFK("jobs", "project_id,job_id", "project_id,id", "CASCADE"), canonicalFK("project_map_entries", "project_id,workstream_id", "project_id,id", "NO ACTION")},
	"project_map_import_sources":        {canonicalFK("projects", "project_id", "id", "CASCADE")},
	"project_map_import_history":        {canonicalFK("project_map_import_sources", "project_id,source_key", "project_id,source_key", "CASCADE")},
	"project_map_import_items":          {canonicalFK("project_map_import_sources", "project_id,source_key", "project_id,source_key", "CASCADE"), canonicalFK("project_map_entry_history", "project_id,entry_id,entry_revision", "project_id,entry_id,revision", "NO ACTION"), canonicalFK("project_map_import_history", "project_id,source_key,source_revision", "project_id,source_key,revision", "NO ACTION")},

	"notes":                           {canonicalFK("projects", "project_id", "id", "CASCADE"), canonicalFK("workers", "project_id,worker_id", "project_id,id", "NO ACTION"), canonicalFK("jobs", "project_id,job_id", "project_id,id", "NO ACTION")},
	"note_revisions":                  {canonicalFK("notes", "project_id,note_id", "project_id,id", "CASCADE"), canonicalFK("workers", "project_id,worker_id", "project_id,id", "NO ACTION"), canonicalFK("jobs", "project_id,job_id", "project_id,id", "NO ACTION")},
	"worker_pause_state":              {canonicalFK("workers", "project_id,worker_id", "project_id,id", "NO ACTION")},
	"worker_pause_history":            {canonicalFK("workers", "project_id,worker_id", "project_id,id", "NO ACTION")},
	"worker_primary_jobs":             {canonicalFK("workers", "project_id,worker_id", "project_id,id", "NO ACTION"), canonicalFK("jobs", "project_id,job_id", "project_id,id", "NO ACTION")},
	"worker_primary_job_history":      {canonicalFK("workers", "project_id,worker_id", "project_id,id", "NO ACTION"), canonicalFK("jobs", "project_id,job_id", "project_id,id", "NO ACTION")},
	"job_updates":                     {canonicalFK("jobs", "project_id,job_id", "project_id,id", "CASCADE")},
	"job_checklists":                  {canonicalFK("jobs", "project_id,job_id", "project_id,id", "CASCADE")},
	"job_checklist_history":           {canonicalFK("jobs", "project_id,job_id", "project_id,id", "CASCADE")},
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
	"review_pull_versions":            {canonicalFK("review_snapshots", "project_id,version_revision", "project_id,revision", "NO ACTION")},
	"review_run_versions":             {canonicalFK("review_snapshots", "project_id,version_revision", "project_id,revision", "NO ACTION")},
	"review_finding_versions":         {canonicalFK("review_run_versions", "project_id,run_id,run_version_revision,pull_number", "project_id,id,version_revision,pull_number", "NO ACTION")},
	"review_attempt_versions":         {canonicalFK("review_snapshots", "project_id,version_revision", "project_id,revision", "NO ACTION")},
	"review_retired_versions":         {canonicalFK("review_snapshots", "project_id,version_revision", "project_id,revision", "NO ACTION")},
	"review_pull_memberships":         {canonicalFK("review_snapshots", "project_id,start_revision", "project_id,revision", "NO ACTION"), canonicalFK("review_snapshots", "project_id,end_revision", "project_id,revision", "NO ACTION"), canonicalFK("review_pull_versions", "project_id,number,version_revision", "project_id,number,version_revision", "NO ACTION")},
	"review_run_memberships":          {canonicalFK("review_snapshots", "project_id,start_revision", "project_id,revision", "NO ACTION"), canonicalFK("review_snapshots", "project_id,end_revision", "project_id,revision", "NO ACTION"), canonicalFK("review_run_versions", "project_id,id,version_revision,pull_number", "project_id,id,version_revision,pull_number", "NO ACTION")},
	"review_attempt_memberships":      {canonicalFK("review_snapshots", "project_id,start_revision", "project_id,revision", "NO ACTION"), canonicalFK("review_snapshots", "project_id,end_revision", "project_id,revision", "NO ACTION"), canonicalFK("review_attempt_versions", "project_id,source,source_attempt_id,version_revision,pull_number", "project_id,source,source_attempt_id,version_revision,pull_number", "NO ACTION")},
	"review_retired_memberships":      {canonicalFK("review_snapshots", "project_id,start_revision", "project_id,revision", "NO ACTION"), canonicalFK("review_snapshots", "project_id,end_revision", "project_id,revision", "NO ACTION"), canonicalFK("review_retired_versions", "project_id,number,version_revision", "project_id,number,version_revision", "NO ACTION")},
	"review_current_pulls":            {canonicalFK("review_pull_memberships", "project_id,number,start_revision,position", "project_id,number,start_revision,position", "NO ACTION")},
	"review_current_runs":             {canonicalFK("review_run_memberships", "project_id,id,start_revision,pull_number,run_order", "project_id,id,start_revision,pull_number,run_order", "NO ACTION")},
	"review_current_attempts":         {canonicalFK("review_attempt_memberships", "project_id,source,source_attempt_id,start_revision,pull_number,position", "project_id,source,source_attempt_id,start_revision,pull_number,position", "NO ACTION")},
	"review_current_retired":          {canonicalFK("review_retired_memberships", "project_id,number,start_revision,position", "project_id,number,start_revision,position", "NO ACTION")},
	"review_current_totals":           {canonicalFK("review_current_pulls", "project_id,pull_number", "project_id,number", "CASCADE")},
	"review_current_attempt_order":    {canonicalFK("review_current_attempts", "project_id,source,source_attempt_id", "project_id,source,source_attempt_id", "CASCADE")},
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
		if count > 64 || rows.Scan(&id, &seq, &parent, &from, &to, &update, &onDelete, &match) != nil || id < 0 || seq < 0 || seq >= 6 || !from.Valid || !to.Valid {
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
