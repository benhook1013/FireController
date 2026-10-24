package backup

import (
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/reviewexec"
	"github.com/benhook1013/FireController/internal/reviewprovider"
	"github.com/benhook1013/FireController/internal/reviewruntime"
)

func TestAllowanceObservationsCanonicalBackupRestore(t *testing.T) {
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
	reply := reviewprovider.IssueComment{ID: 72, Author: q.ProviderActor, CreatedAt: at, UpdatedAt: at, Body: "<!-- This is an auto-generated reply by CodeRabbit -->\nYour [plan](https://docs.coderabbit.ai/management/plans) includes PR reviews subject to [rate limits](https://docs.coderabbit.ai/management/plans#rate-limits). More reviews will be available in 2 minutes."}
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
	if _, err := e.Advance(t.Context(), f.project, attemptID, 1, "held", "hosted_allowance_cooldown", ""); err != nil {
		t.Fatal(err)
	}
	e, err = reviewexec.New(f.store, []reviewexec.Capability{{ID: "restore-cap", Provider: "coderabbit", Account: "fixture-account", Channels: []string{"cli", "hosted"}, Fingerprint: strings.Repeat("f", 64)}})
	if err != nil {
		t.Fatal(err)
	}
	one := int64(1)
	if _, err := e.Configure(t.Context(), scope, "allowance-roundtrip-config", reviewexec.Configuration{ExpectedRevision: &one, CapabilityID: "restore-cap", Mode: "independent", Queue: []int64{2, 4, 6}, Channels: []string{"cli", "hosted"}}); err != nil {
		t.Fatal(err)
	}
	for _, number := range []int64{4, 6} {
		raw, err := e.Enqueue(t.Context(), scope, "allowance-roundtrip-"+string(rune('0'+number)), reviewexec.Request{ExpectedConfigurationRevision: 2, PullNumber: number, HeadSHA: strings.Repeat("a", 40), Channel: "cli", AllowOutOfOrder: number == 4})
		if err != nil {
			t.Fatal(err)
		}
		var receipt reviewexec.Receipt
		if json.Unmarshal(raw, &receipt) != nil {
			t.Fatal("receipt")
		}
		a, err := e.StartAttempt(t.Context(), f.project, receipt.ID)
		if err != nil {
			t.Fatal(err)
		}
		anchor := q.Anchor
		anchor.PullNumber = number
		anchor.ObservedAt = time.Now().UTC().Format(time.RFC3339Nano)
		input := reviewexec.CLIAllowanceObservation{Anchor: anchor, ObservedAt: anchor.ObservedAt, Limit: 3, Remaining: 0, FullCapacityInMS: 0, RollingWindowMS: 3600000, ChangedFiles: 1}
		reason := "allowance_exhausted"
		if number == 4 {
			err = e.RecordCLIExhaustion(t.Context(), f.project, a.ID, 1, input)
		} else {
			input.Remaining = 1
			err = e.RecordCLIAllowance(t.Context(), f.project, a.ID, 1, input)
			reason = "admission_changed"
		}
		if err != nil {
			t.Fatal(err)
		}
		if _, err := e.Advance(t.Context(), f.project, a.ID, 1, "held", reason, ""); err != nil {
			t.Fatal(err)
		}
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
	var explicit bool
	if err := db.QueryRow(`SELECT allow_out_of_order FROM review_operations WHERE pull_number=4 AND channel='cli'`).Scan(&explicit); err != nil || !explicit {
		t.Fatal("restored exhaustion lost explicit ordering choice", explicit, err)
	}
	after := backupRows(t, db)
	for _, table := range []string{"review_cli_preflight", "review_hosted_preflight", "review_hosted_observations", "review_hosted_allowance_queries", "review_operations", "review_execution_attempts", "review_execution_transitions", "receipts"} {
		if after[table] != before[table] {
			t.Fatal("restored allowance facts changed", table)
		}
	}
	if err := reviewexec.ValidateAllowanceSnapshot(t.Context(), db, 7); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`UPDATE review_cli_preflight SET body_json=body_json||' '`,
		`UPDATE review_cli_preflight SET body_json=json_set(body_json,'$.attempt_revision',2)`,
		`UPDATE review_cli_preflight SET body_json=json_set(body_json,'$.observation.remaining',-1)`,
		`UPDATE review_cli_preflight SET body_json=json_set(body_json,'$.project_id','foreign')`,
		`UPDATE review_hosted_preflight SET body_json=replace(body_json,'cooldown_observed','available_observed')`,
		`UPDATE review_hosted_preflight SET body_json=json_set(body_json,'$.input.capture_sha256','bad')`,
	} {
		invalid := restoreModifiedBundle(t, bundle, statement)
		if _, err := Inspect(t.Context(), invalid); err == nil {
			t.Fatal("rehashed malformed allowance accepted", statement)
		}
	}
	if !reflect.DeepEqual(before, backupRows(t, f.store.DB())) || !reflect.DeepEqual(immutable, restoreTree(t, bundle.Path)) {
		t.Fatal("backup/inspection changed original source")
	}
}
