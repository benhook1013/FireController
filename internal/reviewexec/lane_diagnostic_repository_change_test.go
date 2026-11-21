package reviewexec

import (
	"context"
	"database/sql"
	"testing"
)

func TestLaneDiagnosticSuppressesRetainedPrefixAfterRepositoryChange(t *testing.T) {
	f := schedulingFixture(t)
	err := f.store.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
		c, rev, err := readConfiguration(ctx, tx, attemptProject)
		if err != nil {
			return err
		}
		return writeFrontFrame(ctx, tx, Run{ProjectID: attemptProject, Channel: "cli", ConfigurationRevision: rev}, 801, c, runChoice{prefix: []SourcePlan{laneDiagnosticPlan(2)}, sourceReason: "repository_changed"})
	})
	if err != nil {
		t.Fatal(err)
	}
	before := selectionCounts(t, f)
	out, err := f.e.ReadLaneDiagnostic(t.Context(), selectionScope(t, f, attemptProject, f.reader, "read"), "cli")
	if err != nil || out.SourceReason != "repository_changed" || out.SourceComplete || len(out.Candidates) != 2 {
		t.Fatal(out, err)
	}
	for _, candidate := range out.Candidates {
		if candidate.HeadSHA != "" || candidate.SourceObservedAt != "" || candidate.ParentPullNumber != 0 || candidate.LocalState != "unknown" || candidate.LocalReason != "source_not_observed" {
			t.Fatal("repository change must invalidate the complete retained prefix", candidate)
		}
	}
	if selectionCounts(t, f) != before {
		t.Fatal("diagnostic changed selection state")
	}
}
