package backup

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviewexec"
)

func TestHistoricalCoverageMetadataBackupRestoreAndCorruptionRefusal(t *testing.T) {
	for _, tc := range []struct {
		name       string
		limits     []string
		incomplete bool
	}{
		{"seventeen", strings.Fields(strings.Repeat("limitation ", 17)), false},
		{"long", []string{strings.Repeat("a", 1025)}, false},
		{"incomplete-sixteen", strings.Fields(strings.Repeat("limitation ", 16)), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRestoreFixture(t)
			cli, _ := bundleNativeReferences(t, f)
			var body, fingerprint string
			if err := f.store.DB().QueryRow(`SELECT body_json,fingerprint FROM review_execution_evidence`).Scan(&body, &fingerprint); err != nil {
				t.Fatal(err)
			}
			var evidence reviewexec.ReviewEvidence
			if err := json.Unmarshal([]byte(body), &evidence); err != nil {
				t.Fatal(err)
			}
			originalState := backupRows(t, f.store.DB())
			// Seed the predecessor's accepted canonical input shape on the real
			// admitted capture/artifact. Only warning metadata and incomplete outcome
			// differ; do not settle attempts or manufacture review/round credit.
			evidence.Capture.Review.CoverageLimits = tc.limits
			if tc.incomplete {
				evidence.Capture.Review.Complete = false
				evidence.Capture.Review.Outcome = "failed"
			}
			capture, err := json.Marshal(evidence.Capture)
			if err != nil || len(capture) > reviewexec.MaxEvidenceInputBytes || reviewexec.ValidateRetainedReviewCapture(evidence.Capture) != nil {
				t.Fatal("fixture exceeds prior admission contract", err)
			}
			digest := sha256.Sum256(capture)
			fingerprint = hex.EncodeToString(digest[:])
			encoded, err := json.Marshal(evidence)
			if err != nil {
				t.Fatal(err)
			}
			body = string(encoded)
			if _, err = f.store.DB().Exec(`UPDATE review_execution_evidence SET body_json=?,fingerprint=? WHERE id=?`, body, fingerprint, evidence.ID); err != nil {
				t.Fatal(err)
			}
			seeded := backupRows(t, f.store.DB())
			for table, rows := range originalState {
				if table != "review_execution_evidence" && seeded[table] != rows {
					t.Fatal("historical fixture changed admission/accounting", table)
				}
			}
			raw, err := os.ReadFile(filepath.Join(f.dir, cli))
			if err != nil {
				t.Fatal(err)
			}
			bundle := restoreBundle(t, f)
			if _, err := Inspect(t.Context(), bundle.Path); err != nil {
				t.Fatal("historical warnings blocked backup inspection", err)
			}
			destination := filepath.Join(t.TempDir(), "inactive-restored")
			summary, err := Restore(t.Context(), bundle.Path, destination)
			if err != nil || summary.State != "inactive" {
				t.Fatal("historical warnings blocked inactive restore", summary, err)
			}
			restored, err := core.Open(t.Context(), destination)
			if err != nil {
				t.Fatal(err)
			}
			defer restored.Close()
			var retainedBody, retainedFingerprint string
			if err := restored.DB().QueryRow(`SELECT body_json,fingerprint FROM review_execution_evidence WHERE id=?`, evidence.ID).Scan(&retainedBody, &retainedFingerprint); err != nil || retainedBody != body || retainedFingerprint != fingerprint {
				t.Fatal("restore changed original capture/fingerprint", err)
			}
			restoredRaw, err := os.ReadFile(filepath.Join(destination, cli))
			if err != nil || string(restoredRaw) != string(raw) {
				t.Fatal("restore changed original forensic artifact", err)
			}
			if !reflect.DeepEqual(seeded, backupRows(t, f.store.DB())) {
				t.Fatal("backup/restore changed original source")
			}
			for _, bad := range []string{"fingerprint", "anchor", "raw-reference"} {
				var changed reviewexec.ReviewEvidence
				if err := json.Unmarshal([]byte(body), &changed); err != nil {
					t.Fatal(err)
				}
				switch bad {
				case "anchor":
					changed.Capture.ExpectedAnchor.PullNumber = 0
				case "raw-reference":
					changed.Capture.CaptureBytes++
				}
				changedCapture, err := json.Marshal(changed.Capture)
				if err != nil {
					t.Fatal(err)
				}
				changedDigest := sha256.Sum256(changedCapture)
				changedFingerprint := hex.EncodeToString(changedDigest[:])
				if bad == "fingerprint" {
					changedFingerprint = strings.Repeat("0", 64)
				}
				changedBody, err := json.Marshal(changed)
				if err != nil {
					t.Fatal(err)
				}
				mutateBundleSQL(t, bundle.Path, `UPDATE review_execution_evidence SET body_json=?,fingerprint=? WHERE id=?`, string(changedBody), changedFingerprint, evidence.ID)
				_, err = Inspect(t.Context(), bundle.Path)
				backupCode(t, err, "backup_invalid")
				if !reflect.DeepEqual(seeded, backupRows(t, f.store.DB())) {
					t.Fatal("rejected corruption changed source", bad)
				}
			}
		})
	}
}
