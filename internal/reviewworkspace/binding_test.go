package reviewworkspace_test

import (
	"context"
	"testing"
)

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
