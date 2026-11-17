package reviewruntime

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/reviewexec"
)

func TestRecoverHostedRetainedCoverageDisplaySettlesOfflineWithoutChangingSource(t *testing.T) {
	for _, limits := range [][]string{strings.Fields(strings.Repeat("limitation ", 17)), {strings.Repeat("é", 513)}} {
		f, captures, a, reader := recoveryRuntimeFixture(t, "uncertain")
		recoveryRuntimeMetadata(t, f, a)
		retained, err := RetainHosted(t.Context(), captures, f.engine, a, reader.record)
		if err != nil || !retained.Complete {
			t.Fatal("source fixture did not retain complete evidence", retained, err)
		}
		var body string
		if err = f.store.DB().QueryRow(`SELECT body_json FROM review_execution_evidence WHERE id=?`, retained.Receipt.ID).Scan(&body); err != nil {
			t.Fatal(err)
		}
		var evidence reviewexec.ReviewEvidence
		if err = json.Unmarshal([]byte(body), &evidence); err != nil {
			t.Fatal(err)
		}
		// Represent previously admitted source metadata without changing the raw
		// capture, original attempt or current admission limits.
		evidence.Capture.Review.CoverageLimits = limits
		canonical, err := json.Marshal(evidence.Capture)
		if err != nil || reviewexec.ValidateRetainedReviewCapture(evidence.Capture) != nil {
			t.Fatal("invalid retained source fixture", err)
		}
		digest := sha256.Sum256(canonical)
		fingerprint := hex.EncodeToString(digest[:])
		encoded, err := json.Marshal(evidence)
		if err != nil {
			t.Fatal(err)
		}
		body = string(encoded)
		if _, err = f.store.DB().Exec(`UPDATE review_execution_evidence SET body_json=?,fingerprint=? WHERE id=?`, body, fingerprint, evidence.ID); err != nil {
			t.Fatal(err)
		}
		rawPath := filepath.Join(captures.root, hostedCaptureFilename(a.ID, retained.Capture))
		rawBefore, err := os.ReadFile(rawPath)
		if err != nil {
			t.Fatal(err)
		}
		reader.failure = errors.New("provider is offline")
		result, err := recoverRuntime(t, f, captures, a, reader)
		if err != nil || result.State != "completed" || result.EvidenceID != evidence.ID || len(reader.calls) != 0 || recoveryRuntimeCount(t, f, "review_execution_reservations") != 0 {
			t.Fatal("display warning policy blocked original offline recovery", result, err, reader.calls)
		}
		again, err := recoverRuntime(t, f, captures, a, reader)
		if err != nil || again.State != "completed" || again.Reason != "already_completed" || again.EvidenceID != evidence.ID || len(reader.calls) != 0 || recoveryRuntimeCount(t, f, "review_execution_evidence") != 1 || recoveryRuntimeCount(t, f, "review_hosted_observations") != 1 || recoveryRuntimeCount(t, f, "review_adjudication_state") != 0 {
			t.Fatal("recovery replay invented another observation, source or finalization", again, err)
		}
		var afterBody, afterFingerprint string
		if err = f.store.DB().QueryRow(`SELECT body_json,fingerprint FROM review_execution_evidence WHERE id=?`, evidence.ID).Scan(&afterBody, &afterFingerprint); err != nil || afterBody != body || afterFingerprint != fingerprint {
			t.Fatal("recovery changed immutable evidence metadata", err)
		}
		rawAfter, err := os.ReadFile(rawPath)
		if err != nil || string(rawAfter) != string(rawBefore) {
			t.Fatal("recovery changed the original raw capture", err)
		}
	}
}
