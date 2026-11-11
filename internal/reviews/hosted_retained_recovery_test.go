package reviews

import (
	"context"
	"database/sql"
	"github.com/benhook1013/FireController/internal/core"
	"strings"
	"testing"
)

func TestRetainedHostedRecoveryDoesNotRelaxOrdinaryPublication(t *testing.T) {
	for _, kind := range []string{"qualified", "ordinary", "other failure", "cancelled", "cli", "changed head", "changed trigger", "existing different evidence"} {
		t.Run(kind, func(t *testing.T) {
			f := newAPIFixture(t)
			a := nativeAttempt("original-hosted-attempt", "failed")
			a.Channel = "hosted"
			a.FinishedAt = "2026-01-01T00:02:00Z"
			a.Reason = "hosted_finished_incomplete"
			a.TriggerID = "101"
			switch kind {
			case "other failure":
				a.Reason = "other_failure"
			case "cancelled":
				a.State = "cancelled"
				a.Reason = ""
			case "cli":
				a.Channel = "cli"
			}
			if kind == "existing different evidence" {
				prior := nativeRun("prior-evidence")
				prior.Channel = "hosted"
				mustNativePublish(t, f, NativePublication{Pull: nativePull(1), Run: &prior})
				a.SourceRunID = prior.SourceRunID
			}
			failed := mustNativePublish(t, f, NativePublication{Pull: nativePull(1), Attempt: &a})
			before := nativeSnapshotBytes(t, f, failed.ID)
			run := nativeRun("recovered-evidence")
			run.Channel = a.Channel
			mustNativePublish(t, f, NativePublication{Pull: nativePull(1), Run: &run})
			a.State = "completed"
			a.Reason = ""
			a.SourceRunID = run.SourceRunID
			a.FinishedAt = "2026-01-01T00:03:00Z"
			if kind == "changed head" {
				a.HeadSHA = strings.Repeat("f", 40)
			}
			if kind == "changed trigger" {
				a.TriggerID = "202"
			}
			err := f.store.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
				p := NativePublication{Pull: nativePull(1), Attempt: &a}
				scope := core.Scope{ProjectID: f.project, PrincipalID: "synthetic-native-publisher"}
				if kind == "ordinary" {
					_, err := PublishNative(ctx, tx, scope, p)
					return err
				}
				_, err := PublishRecoveredHostedAttempt(ctx, tx, scope, p)
				return err
			})
			if kind == "qualified" && err != nil {
				t.Fatal(err)
			}
			if kind != "qualified" && err == nil {
				t.Fatal("unqualified terminal history reopened")
			}
			if after := nativeSnapshotBytes(t, f, failed.ID); after != before {
				t.Fatal("prior failed snapshot rewritten")
			}
		})
	}
}
func TestRetainedHostedRecoveryTokenScope(t *testing.T) {
	a := Attempt{SourceAttemptID: "original", SourceRunID: "evidence", Channel: "hosted", State: "completed"}
	token := retainedHostedRecovery{project: "project", attempt: "original", evidence: "evidence"}
	ctx := context.WithValue(t.Context(), retainedHostedRecoveryKey{}, token)
	if !allowsRetainedHostedRecovery(ctx, "project", nativeSource, "failed", "hosted_finished_incomplete", "", a) {
		t.Fatal("matching recovery denied")
	}
	for _, change := range []string{"project", "attempt", "evidence", "source", "old run", "state", "reason"} {
		project, source, state, reason, run := "project", nativeSource, "failed", "hosted_finished_incomplete", ""
		copy := a
		switch change {
		case "project":
			project = "other"
		case "attempt":
			copy.SourceAttemptID = "other"
		case "evidence":
			copy.SourceRunID = "other"
		case "source":
			source = "imported"
		case "old run":
			run = "old"
		case "state":
			state = "cancelled"
		case "reason":
			reason = "other_failure"
		}
		if allowsRetainedHostedRecovery(ctx, project, source, state, reason, run, copy) {
			t.Fatal("unscoped token", change)
		}
	}
	if allowsRetainedHostedRecovery(t.Context(), "project", nativeSource, "failed", "hosted_finished_incomplete", "", a) {
		t.Fatal("ordinary context allowed recovery")
	}
}
