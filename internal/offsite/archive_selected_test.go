package offsite

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOffsitePackingReusesVerifiedSelection(t *testing.T) {
	f := newFixture(t)
	f.bundle(t)
	selected, err := newest(t.Context(), f.c)
	if err != nil || selected.State != "verified" {
		t.Fatal("fixture lacks verified source selection", selected, err)
	}
	archive := filepath.Join(t.TempDir(), "selected.tar.gz")
	got, err := packVerified(t.Context(), selected, archive, f.c)
	if err != nil {
		t.Fatal(err)
	}
	reference := filepath.Join(t.TempDir(), "independent.tar.gz")
	want, err := pack(t.Context(), selected.Path, reference, f.c)
	if err != nil || got != want {
		t.Fatal("reused selection changed the verified receipt", got, want, err)
	}
	a, err := os.ReadFile(archive)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(reference)
	if err != nil || !bytes.Equal(a, b) {
		t.Fatal("reused selection changed deterministic archive bytes", err)
	}
	// Verification follows the captured bytes, even after local retention removes
	// the selected source. No second source inspection can supply that proof.
	if err := os.RemoveAll(selected.Path); err != nil {
		t.Fatal(err)
	}
	summary, err := unpack(t.Context(), archive, t.TempDir(), got, f.c)
	if err != nil || summary.ID != selected.ID || summary.SourceInstanceID != selected.SourceInstanceID || summary.EventSequence != selected.EventSequence {
		t.Fatal("prepared archive lost independent native verification", summary, err)
	}
}

func TestOffsiteSelectedPackingPreservesPreparedVerificationAndGuards(t *testing.T) {
	f := newFixture(t)
	f.bundle(t)
	selected, err := newest(t.Context(), f.c)
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"foreign-instance", "cancelled"} {
		t.Run(mode, func(t *testing.T) {
			candidate := selected
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if mode == "foreign-instance" {
				candidate.SourceInstanceID = "instance_" + strings.Repeat("f", 32)
			} else {
				cancel()
			}
			destination := filepath.Join(t.TempDir(), "absent.tar.gz")
			_, err := packVerified(ctx, candidate, destination, f.c)
			code := "offsite_source_invalid"
			if mode == "cancelled" {
				code = "offsite_cancelled"
			}
			wantCode(t, err, code)
			if _, err := os.Lstat(destination); !os.IsNotExist(err) {
				t.Fatal("refused selection created an archive", err)
			}
		})
	}
	// The summary cannot vouch for source bytes that changed after selection.
	// Packing captures them; native inspection of the prepared archive must fail.
	database := filepath.Join(selected.Path, "firecontroller.sqlite")
	body, err := os.ReadFile(database)
	if err != nil || len(body) == 0 {
		t.Fatal("missing selected database", err)
	}
	body[0] ^= 0xff
	if err := os.WriteFile(database, body, 0600); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "changed.tar.gz")
	receipt, err := packVerified(t.Context(), selected, archive, f.c)
	if err != nil {
		t.Fatal("fixture did not capture changed source bytes", err)
	}
	_, err = unpack(t.Context(), archive, t.TempDir(), receipt, f.c)
	wantCode(t, err, "offsite_archive_invalid")
	_, err = pack(t.Context(), selected.Path, filepath.Join(t.TempDir(), "unverified.tar.gz"), f.c)
	wantCode(t, err, "offsite_source_invalid")
}
