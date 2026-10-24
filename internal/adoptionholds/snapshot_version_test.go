package adoptionholds_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	h "github.com/benhook1013/FireController/internal/adoptionholds"
	"github.com/benhook1013/FireController/internal/core"
)

func TestSnapshotRequiresCanonicalDomainVersion(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value any
		valid bool
	}{
		{"canonical", "1", true},
		{"noncanonical-leading-zero", "01", false},
		{"noncanonical-plus", "+1", false},
		{"noncanonical-leading-space", " 1", false},
		{"noncanonical-trailing-space", "1 ", false},
		{"noncanonical-decimal", "1.0", false},
		{"malformed", "broken", false},
		{"empty", "", false},
		{"unsupported", "2", false},
		{"blob", []byte("1"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// An empty installed domain must enforce the same version guard as
			// scoped runtime reads, without relying on a later source traversal.
			f := fresh(t)
			f.exec(`UPDATE metadata SET value=? WHERE key='schema:review_adoption_obligations'`, tc.value)
			_, readErr := h.ReadFacts(t.Context(), f.store.DB(), "project_one", 3251)
			snapshotErr := h.ValidateSnapshot(t.Context(), f.store.DB())
			for name, err := range map[string]error{"runtime": readErr, "snapshot": snapshotErr} {
				if tc.valid {
					if err != nil {
						t.Fatalf("%s refused canonical TEXT version: %v", name, err)
					}
					continue
				}
				var failure *core.Error
				if !errors.As(err, &failure) || failure.Code != "storage_unavailable" {
					t.Fatalf("%s accepted or misclassified stored version: %v", name, err)
				}
			}
		})
	}
}

func TestSnapshotVersionPreservesOptionalAbsenceAndQueryErrors(t *testing.T) {
	old, err := core.Open(t.Context(), filepath.Join(t.TempDir(), "without-domain"))
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	if err := h.ValidateSnapshot(t.Context(), old.DB()); err != nil {
		t.Fatal("genuinely absent optional domain refused", err)
	}
	if _, err := old.DB().Exec(`CREATE TABLE review_adoption_partial(id TEXT)`); err != nil {
		t.Fatal(err)
	}
	var failure *core.Error
	if err := h.ValidateSnapshot(t.Context(), old.DB()); !errors.As(err, &failure) || failure.Code != "storage_unavailable" {
		t.Fatal("partial optional domain accepted", err)
	}
	f := fresh(t)
	f.apply(batch(), "canonical-snapshot-version")
	if err := h.ValidateSnapshot(t.Context(), f.store.DB()); err != nil {
		t.Fatal("populated canonical domain refused", err)
	}
	q := obligationQueryFailure{Query: f.store.DB(), match: "schema:review_adoption_obligations", row: true}
	err = h.ValidateSnapshot(t.Context(), q)
	var driverCode interface{ Code() int }
	if !errors.As(err, &driverCode) || driverCode.Code()&255 != 1 {
		t.Fatal("operational version query failure became retained corruption", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := h.ValidateSnapshot(ctx, f.store.DB()); !errors.Is(err, context.Canceled) {
		t.Fatal("version query erased cancellation", err)
	}
}
