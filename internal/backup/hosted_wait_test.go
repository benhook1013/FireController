package backup

import (
	"github.com/benhook1013/FireController/internal/reviewexec"
	"github.com/benhook1013/FireController/internal/reviewprovider"
	"github.com/benhook1013/FireController/internal/reviewruntime"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestHostedWaitCanonicalBackupRestore(t *testing.T) {
	for _, duration := range []string{"2 minutes", "28 seconds"} {
		t.Run(duration, func(t *testing.T) { testHostedWaitCanonicalBackupRestore(t, duration) })
	}
}

func testHostedWaitCanonicalBackupRestore(t *testing.T, duration string) {
	f := newRestoreFixture(t)
	e, err := reviewexec.New(f.store, []reviewexec.Capability{{ID: "restore-cap", Provider: "coderabbit", Account: "fixture-account", Channels: []string{"hosted"}, Fingerprint: strings.Repeat("f", 64)}})
	if err != nil {
		t.Fatal(err)
	}
	scope := routePolicyInventoryScope(t, f)
	var attemptID string
	if err := f.store.DB().QueryRow(`SELECT id FROM review_execution_attempts WHERE state='preparing'`).Scan(&attemptID); err != nil {
		t.Fatal(err)
	}
	q, err := e.ReadHostedAllowanceQuery(t.Context(), f.project, attemptID)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC()
	queryAt, _ := time.Parse(time.RFC3339Nano, q.EarliestPosting)
	queryAt = queryAt.Truncate(time.Second)
	queryComment := reviewprovider.IssueComment{ID: 71, Body: q.Intent, Author: q.PostingActor, CreatedAt: queryAt, UpdatedAt: queryAt}
	q, err = e.ConfirmHostedAllowanceQuery(t.Context(), f.project, attemptID, q.Revision, queryComment)
	if err != nil {
		t.Fatal(err)
	}
	reply := reviewprovider.IssueComment{ID: 72, Author: q.ProviderActor, CreatedAt: at, UpdatedAt: at, Body: "<!-- This is an auto-generated reply by CodeRabbit -->\nYour [plan](https://docs.coderabbit.ai/management/plans) includes PR reviews subject to [rate limits](https://docs.coderabbit.ai/management/plans#rate-limits). More reviews will be available in " + duration + "."}
	now := time.Now().UTC()
	intent := reviewprovider.HostedTriggerIntent{OperationID: q.OperationID, AttemptID: q.AttemptID, HeadSHA: q.Anchor.CandidateSHA}
	snapshot := reviewprovider.HostedPublicationSnapshot{Complete: true, ObservedAt: now, IssueComments: []reviewprovider.IssueComment{queryComment, reply}, Reviews: []reviewprovider.HostedReview{}, ReviewComments: []reviewprovider.ReviewComment{}}
	observation, err := reviewprovider.ResolveHostedAllowance(reviewprovider.HostedAllowanceSnapshot{IssueComments: snapshot.IssueComments, Complete: true, ObservedAt: now}, intent, q.PostingActor, q.ProviderActor, queryAt)
	if err != nil || observation.State != "cooldown_observed" {
		t.Fatal("synthetic actual parsed cooldown", observation, err)
	}
	captures, err := reviewruntime.NewHostedCaptureStore(filepath.Join(f.dir, "hosted-review-captures"))
	if err != nil {
		t.Fatal(err)
	}
	ref, err := captures.Save(t.Context(), attemptID, reviewruntime.HostedCaptureRecord{RepositoryID: q.Anchor.RepositoryID, Repository: q.Anchor.Repository, PullNumber: q.Anchor.PullNumber, Intent: intent, PostingActor: q.PostingActor, ProviderActor: q.ProviderActor, EarliestPosting: queryAt, StartedAt: queryAt, FinishedAt: now, Snapshot: snapshot})
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.RecordHostedObservation(t.Context(), f.project, attemptID, reviewexec.HostedObservationInput{Kind: "allowance_query", CaptureID: ref.ID, CaptureSHA256: ref.SHA256, CaptureBytes: ref.Bytes, StartedAt: queryAt.Format(time.RFC3339Nano), FinishedAt: now.Format(time.RFC3339Nano), ObservedAt: now.Format(time.RFC3339Nano), ExpectedAnchor: q.Anchor, Intent: q.Intent})
	if err != nil {
		t.Fatal(err)
	}
	in := reviewexec.HostedAllowanceInput{QueryID: q.ID, Anchor: q.Anchor, ObservedAt: now.Format(time.RFC3339Nano), ChangedFiles: 1, CaptureID: ref.ID, CaptureSHA256: ref.SHA256, CaptureBytes: ref.Bytes, Observation: observation}
	if err := e.RecordHostedCooldown(t.Context(), f.project, attemptID, 1, in); err != nil {
		t.Fatal(err)
	}

	if _, err := e.ScheduleHostedCooldown(t.Context(), f.project, attemptID, 1); err != nil {
		t.Fatal(err)
	}
	op, err := e.ReadOperation(t.Context(), scope, q.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.ControlOperation(t.Context(), scope, "pause-backup-wait", op.ID, reviewexec.OperationControl{ExpectedRevision: op.Revision, Action: "pause"}); err != nil {
		t.Fatal(err)
	}
	before := backupRows(t, f.store.DB())
	bundle := restoreBundle(t, f)
	immutable := restoreTree(t, bundle.Path)
	if _, err := Inspect(t.Context(), bundle.Path); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "inactive")
	if _, err := Restore(t.Context(), bundle.Path, destination); err != nil {
		t.Fatal(err)
	}
	db, err := openSnapshot(t.Context(), filepath.Join(destination, "firecontroller.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	after := backupRows(t, db)
	for _, table := range []string{"review_operation_waits", "review_execution_attempts", "review_execution_transitions", "review_hosted_preflight", "review_hosted_allowance_queries", "review_hosted_observations", "review_execution_reservations", "review_operations"} {
		if after[table] != before[table] {
			t.Fatal("restored timer facts changed", table)
		}
	}
	for _, statement := range []string{
		`UPDATE review_operation_waits SET retry_not_before='2030-01-01T00:00:00Z'`,
		`UPDATE review_operation_waits SET baseline_attempt_id=NULL,retry_not_before=''`,
		`UPDATE review_operations SET attempt_id=(SELECT baseline_attempt_id FROM review_operation_waits LIMIT 1) WHERE id=(SELECT operation_id FROM review_operation_waits LIMIT 1)`,
	} {
		invalid := restoreModifiedBundle(t, bundle, statement)
		if _, err := Inspect(t.Context(), invalid); err == nil {
			t.Fatal("rehashed malformed timer accepted", statement)
		}
	}
	if !reflect.DeepEqual(before, backupRows(t, f.store.DB())) || !reflect.DeepEqual(immutable, restoreTree(t, bundle.Path)) {
		t.Fatal("timer proof altered source or bundle")
	}
}

// Daemon startup validates the source after schema initialization. Rebuilding
// a parent table removes its attached triggers even when every row survives.
func TestHostedWaitSchemaRepairPassesDaemonBackupStartup(t *testing.T) {
	f := newRestoreFixture(t)
	if err := ValidateSource(t.Context(), f.dir); err != nil {
		t.Fatal("canonical source", err)
	}
	var names []string
	rows, err := f.store.DB().Query(`SELECT name FROM sqlite_schema WHERE type='trigger' AND tbl_name='review_execution_attempts' ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
	}
	rows.Close()
	if len(names) != 3 {
		t.Fatal("canonical attempt triggers", names)
	}
	for _, name := range names {
		if _, err := f.store.DB().Exec(`DROP TRIGGER ` + name); err != nil {
			t.Fatal(err)
		}
	}
	if err := ValidateSource(t.Context(), f.dir); err == nil {
		t.Fatal("startup accepted incomplete schema")
	}
	if err := reviewexec.EnsureSchema(t.Context(), f.store); err != nil {
		t.Fatal("canonical schema repair", err)
	}
	if err := ValidateSource(t.Context(), f.dir); err != nil {
		t.Fatal("repaired source cannot start daemon", err)
	}
}
