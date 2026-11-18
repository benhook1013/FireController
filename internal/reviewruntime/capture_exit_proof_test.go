package reviewruntime

import (
	"github.com/benhook1013/FireController/internal/reviewprocess"
	"testing"
)

func TestCaptureExitProofDurableAndLegacyUnchanged(t *testing.T) {
	store, _ := newTestCaptureStore(t)
	for _, ended := range []bool{false, true} {
		id := "legacy-proof"
		if ended {
			id = "ended-proof"
		}
		record := captureTestRecord(reviewprocess.Capture{Stdout: []byte("invalid provider response"), Started: true, ExitCode: 0, Exited: ended})
		ref, err := store.Save(t.Context(), id, record)
		if err != nil {
			t.Fatal(err)
		}
		got, loaded, err := store.Read(t.Context(), id)
		if err != nil {
			t.Fatal(err)
		}
		if got.Capture.Exited != ended || loaded != ref {
			t.Fatal("exit proof not preserved")
		}
		replay, err := store.Save(t.Context(), id, got)
		if err != nil || replay != ref {
			t.Fatal("immutable replay changed", err)
		}
	}
}
