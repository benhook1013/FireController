package reviewworkspace_test

import (
	"context"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/reviewworkspace"
)

func TestCanonicalBindingRejectsNilManagerAndContext(t *testing.T) {
	f := newFixture(t)
	w := prepare(t, f, "nil_binding")
	origin := git(t, f.git, w.Directory, "remote", "get-url", "origin")
	for _, input := range []struct {
		name    string
		manager *reviewworkspace.Manager
		ctx     context.Context
	}{
		{"nil_manager", nil, context.Background()},
		{"nil_context", f.manager, nil},
		{"both_nil", nil, nil},
	} {
		t.Run(input.name, func(t *testing.T) {
			if _, err := input.manager.BindRepository(input.ctx, w, "owner/project"); err == nil {
				t.Fatal("nil binding argument accepted")
			}
			if got := git(t, f.git, w.Directory, "remote", "get-url", "origin"); got != origin {
				t.Fatal("invalid binding changed origin")
			}
		})
	}
	if err := f.manager.Verify(context.Background(), w); err != nil {
		t.Fatal("nil argument changed pinned workspace", err)
	}
}

func TestCanonicalBindingRespectsCancelledAndExpiredParent(t *testing.T) {
	f := newFixture(t)
	w := prepare(t, f, "cancelled_binding")
	origin := git(t, f.git, w.Directory, "remote", "get-url", "origin")
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	expired, stop := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer stop()
	for _, input := range []struct {
		name string
		ctx  context.Context
	}{{"cancelled", cancelled}, {"expired", expired}} {
		t.Run(input.name, func(t *testing.T) {
			if _, err := f.manager.BindRepository(input.ctx, w, "owner/project"); err == nil {
				t.Fatal("binding ignored terminated parent context")
			}
			if got := git(t, f.git, w.Directory, "remote", "get-url", "origin"); got != origin {
				t.Fatal("cancelled binding changed origin")
			}
		})
	}
	if err := f.manager.Verify(context.Background(), w); err != nil {
		t.Fatal("cancelled binding changed pinned workspace", err)
	}
}

func TestCanonicalBindingPreservesPinnedSourceAndRejectsOriginDrift(t *testing.T) {
	f := newFixture(t)
	original := prepare(t, f, "canonical_binding")
	w, err := f.manager.BindRepository(context.Background(), original, "owner/project")
	if err != nil {
		t.Fatal(err)
	}
	if w.RepositoryCanonical != "owner/project" || w.CandidateSHA != original.CandidateSHA || w.BaseSHA != original.BaseSHA || w.MergeBaseSHA != original.MergeBaseSHA || w.PatchSHA256 != original.PatchSHA256 {
		t.Fatal("binding changed source identity")
	}
	if got := git(t, f.git, w.Directory, "remote", "get-url", "origin"); got != "https://github.com/owner/project.git" {
		t.Fatal("origin differs from verified provider")
	}
	if err = f.manager.Verify(context.Background(), w); err != nil {
		t.Fatal(err)
	}
	git(t, f.git, w.Directory, "remote", "set-url", "origin", "https://example.invalid/other.git")
	if f.manager.Verify(context.Background(), w) == nil {
		t.Fatal("changed origin accepted")
	}
}

func TestInvalidCanonicalBindingHasNoConfigurationEffects(t *testing.T) {
	f := newFixture(t)
	w := prepare(t, f, "invalid_binding")
	origin := git(t, f.git, w.Directory, "remote", "get-url", "origin")
	for _, canonical := range []string{"", "../project", "owner/..", "owner/project?token=secret", "https://github.com/owner/project", "owner/project/extra"} {
		if _, err := f.manager.BindRepository(context.Background(), w, canonical); err == nil {
			t.Fatal("invalid canonical repository accepted")
		}
		if got := git(t, f.git, w.Directory, "remote", "get-url", "origin"); got != origin {
			t.Fatal("invalid binding changed origin")
		}
	}
}
