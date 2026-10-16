package reviewruntime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviewexec"
	"github.com/benhook1013/FireController/internal/reviews"
	"github.com/benhook1013/FireController/internal/reviewworkspace"
)

func workspacePreparationInputs(t *testing.T, f *cliDriverFixture) (reviewexec.ExecutionContext, reviewexec.SourcePlan, reviewexec.Attempt) {
	t.Helper()
	op, a, _ := f.start(t, 2, 1, "shared-preparation")
	current, err := f.engine.ExecutionContext(t.Context(), f.project, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	plan := reviewexec.SourcePlan{Repository: f.reader.repository, Pull: f.reader.pulls[2], ParentSHA: f.baseSHA, ObservedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	return current, plan, a
}

func TestPrepareReviewWorkspaceBoundGitCountAndNoProviderOrLedgerWork(t *testing.T) {
	f := newCLIDriverFixture(t)
	current, plan, a := workspacePreparationInputs(t, f)
	var before int
	if err := f.store.DB().QueryRow("SELECT count(*) FROM events").Scan(&before); err != nil {
		t.Fatal(err)
	}
	var order []string
	fetch := func(ctx context.Context, id int64, repository, head, base, credential string) error {
		order = append(order, "fetch")
		if id != current.RepositoryID || repository != plan.Repository.FullName || head != plan.Pull.HeadSHA || base != plan.Pull.BaseSHA || credential != f.capability.GitHubCredential() {
			t.Fatal("fetch binding changed")
		}
		return nil // Real commits already exist in this fixture's local cache.
	}
	verify := func(ctx context.Context, w reviewworkspace.Workspace) error {
		order = append(order, "verify")
		if w.RepositoryCanonical != plan.Repository.FullName || w.CandidateSHA != plan.Pull.HeadSHA || w.BaseSHA != plan.Pull.BaseSHA {
			t.Fatal("verification ran before canonical binding")
		}
		return f.manager.Verify(ctx, w)
	}
	w, count, err := prepareReviewWorkspace(t.Context(), fetch, verify, f.manager, f.capability.GitHubCredential(), current, plan, a.ID)
	if err != nil || count != 2 || w.MergeBaseSHA != f.baseSHA || len(w.PatchSHA256) != 64 || !reflect.DeepEqual(order, []string{"fetch", "verify"}) {
		t.Fatal(w, count, order, err)
	}
	// Provider's coarse ChangedFiles=999 is deliberately unrelated to actual Git count.
	if plan.Pull.ChangedFiles != 999 {
		t.Fatal("fixture no longer distinguishes provider estimate")
	}
	if err = f.manager.Verify(t.Context(), w); err != nil {
		t.Fatal(err)
	}
	var after int
	if err = f.store.DB().QueryRow("SELECT count(*) FROM events").Scan(&after); err != nil || after != before || f.reader.repositoryCalls != 0 || len(f.reader.calls) != 0 {
		t.Fatal("shared materialization read provider or wrote ledger", err)
	}
	if _, err = os.Stat(f.log); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("shared materialization ran CLI", err)
	}
}

func TestPrepareReviewWorkspaceStopsAtOriginalGuardAndRetainsSafeReason(t *testing.T) {
	for _, tc := range []struct {
		name, reason string
		verify       bool
	}{
		{"fetch", "source_unavailable", false}, {"missing_commit", "workspace_unavailable", false}, {"repository_binding", "workspace_unavailable", false}, {"verify", "workspace_changed", true}, {"empty_comparison", "comparison_unavailable", true}, {"dirty_after_verify", "comparison_unavailable", true}, {"file_ceiling", "project_file_limit", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newCLIDriverFixture(t)
			current, plan, a := workspacePreparationInputs(t, f)
			switch tc.name {
			case "missing_commit":
				plan.Pull.HeadSHA = strings.Repeat("f", 40)
			case "repository_binding":
				plan.Repository.FullName = "invalid"
			case "empty_comparison":
				plan.Pull.HeadSHA = f.baseSHA
			case "file_ceiling":
				current.Policy.Policy.MaxReviewFiles = 1
			}
			fetches, verifications := 0, 0
			fetch := func(context.Context, int64, string, string, string, string) error {
				fetches++
				if tc.name == "fetch" {
					return errors.New("private transport details")
				}
				return nil
			}
			verify := func(ctx context.Context, w reviewworkspace.Workspace) error {
				verifications++
				if tc.name == "verify" {
					return errors.New("private workspace details")
				}
				if tc.name == "dirty_after_verify" {
					return os.WriteFile(filepath.Join(w.Directory, "untracked.txt"), []byte("owned test mutation"), 0600)
				}
				return f.manager.Verify(ctx, w)
			}
			w, count, err := prepareReviewWorkspace(t.Context(), fetch, verify, f.manager, "private-token", current, plan, a.ID)
			var failure *core.Error
			if !errors.As(err, &failure) || failure.Code != "review_held" || failure.Details.(map[string]string)["reason"] != tc.reason || strings.Contains(err.Error(), "private") || w != (reviewworkspace.Workspace{}) || count != 0 {
				t.Fatal("guard result", w, count, err)
			}
			expected := 0
			if tc.verify {
				expected = 1
			}
			if fetches != 1 || verifications != expected || f.reader.repositoryCalls != 0 || len(f.reader.calls) != 0 {
				t.Fatal("later guard/provider work ran", fetches, verifications)
			}
			if _, err = os.Stat(f.log); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("failed workspace preparation ran allowance/review", err)
			}
		})
	}
}

func TestPublishPreparedAttemptFallbackIsDisplayOnlyAndKeepsCurrentFacts(t *testing.T) {
	f := newCLIDriverFixture(t)
	_, plan, a := workspacePreparationInputs(t, f)
	original := strings.Repeat("界", 300)
	plan.Pull.Title = "  " + original + "  "
	if err := publishPreparedAttempt(t.Context(), f.engine, f.project, a.ID, plan); err != nil {
		t.Fatal(err)
	}
	read := func() (reviews.PullInput, int) {
		t.Helper()
		var p reviews.PullInput
		var count int
		err := f.store.DB().QueryRow(`SELECT p.number,p.title,p.head_sha,p.base_sha,p.base_ref,p.ci_state,p.mergeability,p.complete FROM review_pulls p JOIN review_workspace w ON p.project_id=w.project_id AND p.snapshot_id=w.snapshot_id WHERE p.project_id=? AND p.number=2`, f.project).Scan(&p.Number, &p.Title, &p.HeadSHA, &p.BaseSHA, &p.BaseRef, &p.CIState, &p.Mergeability, &p.Complete)
		if err != nil {
			t.Fatal(err)
		}
		if err = f.store.DB().QueryRow("SELECT count(*) FROM review_snapshots").Scan(&count); err != nil {
			t.Fatal(err)
		}
		return p, count
	}
	first, snapshots := read()
	if first.Title != pullDisplayTitle(original) || len(first.Title) > 500 || !utf8.ValidString(first.Title) || first.HeadSHA != plan.Pull.HeadSHA || first.BaseSHA != plan.Pull.BaseSHA || first.BaseRef != plan.Pull.BaseRef || first.CIState != "unknown" || first.Mergeability != "unknown" || first.Complete || plan.Pull.Title != "  "+original+"  " {
		t.Fatal("fallback changed source or invented complete metadata", first)
	}
	// Existing current facts take precedence, including over a different source plan.
	plan.Pull.Title = "Different fetched title"
	plan.Pull.HeadSHA = strings.Repeat("9", 40)
	if err := publishPreparedAttempt(t.Context(), f.engine, f.project, a.ID, plan); err != nil {
		t.Fatal(err)
	}
	again, after := read()
	if !reflect.DeepEqual(first, again) || after != snapshots {
		t.Fatal("replay replaced current facts or created snapshot", first, again)
	}
	// A real projection identity conflict is not repaired by fallback metadata.
	if _, err := f.store.DB().Exec(`UPDATE review_attempts SET head_sha=? WHERE project_id=?`, strings.Repeat("f", 40), f.project); err != nil {
		t.Fatal(err)
	}
	assertCoreErrorCode(t, publishPreparedAttempt(t.Context(), f.engine, f.project, a.ID, plan), "observation_conflict")
	_, after = read()
	if after != snapshots {
		t.Fatal("conflict published replacement snapshot")
	}
	if f.reader.repositoryCalls != 0 || len(f.reader.calls) != 0 {
		t.Fatal("publication helper read provider")
	}
}

func TestPublishPreparedAttemptPropagatesStorageFailure(t *testing.T) {
	f := newCLIDriverFixture(t)
	_, plan, a := workspacePreparationInputs(t, f)
	// Missing tables are corruption/read failures, not the named absent-current-PR result.
	if _, err := f.store.DB().Exec(`DROP TABLE review_workspace`); err != nil {
		t.Fatal(err)
	}
	err := publishPreparedAttempt(t.Context(), f.engine, f.project, a.ID, plan)
	if err == nil {
		t.Fatal("storage failure permitted metadata fallback")
	}
	var snapshots int
	if e := f.store.DB().QueryRow("SELECT count(*) FROM review_snapshots").Scan(&snapshots); e != nil || snapshots != 0 {
		t.Fatal("storage failure created projection", e)
	}
}
