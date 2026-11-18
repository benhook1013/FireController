package backup

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviewexec"
	"github.com/benhook1013/FireController/internal/reviewprovider"
)

func seedRecoveredCLI(t *testing.T, f *restoreFixture) {
	t.Helper()
	bundleNativeReferences(t, f)
	var body string
	if err := f.store.DB().QueryRow(`SELECT body_json FROM review_execution_evidence`).Scan(&body); err != nil {
		t.Fatal(err)
	}
	var next reviewexec.ReviewEvidence
	if err := json.Unmarshal([]byte(body), &next); err != nil {
		t.Fatal(err)
	}
	old := next
	old.Capture.Review = reviewprovider.ParsedReview{Outcome: "uncertain", Findings: []reviewprovider.ParsedFinding{}, ReviewedFiles: []string{}}
	ob, _ := json.Marshal(old)
	oc, _ := json.Marshal(old.Capture)
	nc, _ := json.Marshal(next.Capture)
	a := sha256.Sum256(oc)
	z := sha256.Sum256(nc)
	prior, current := hex.EncodeToString(a[:]), hex.EncodeToString(z[:])
	at := "2026-01-02T00:00:00Z"
	next.Recovery = &reviewexec.EvidenceRecovery{PriorFingerprint: prior, RecoveredAt: at, Reason: "cli_capture_reparsed"}
	nb, _ := json.Marshal(next)
	if _, err := f.store.DB().Exec(`UPDATE review_execution_evidence SET body_json=?,fingerprint=? WHERE id=?`, string(nb), current, next.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.DB().Exec(`INSERT INTO review_cli_evidence_recovery VALUES(?,?,?,?,?,?)`, next.ID, next.ProjectID, string(ob), prior, current, at); err != nil {
		t.Fatal(err)
	}
}
func TestCLIRecoveryArchiveBackupRestore(t *testing.T) {
	f := newRestoreFixture(t)
	seedRecoveredCLI(t, f)
	bundle := restoreBundle(t, f)
	if _, e := Inspect(t.Context(), bundle.Path); e != nil {
		t.Fatal(e)
	}
	dest := filepath.Join(t.TempDir(), "restored")
	if _, e := Restore(t.Context(), bundle.Path, dest); e != nil {
		t.Fatal(e)
	}
	s, e := core.Open(t.Context(), dest)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	var n int
	if e = s.DB().QueryRow(`SELECT count(*) FROM review_cli_evidence_recovery`).Scan(&n); e != nil || n != 1 {
		t.Fatal(n, e)
	}
	if _, e = s.DB().Exec(`DELETE FROM review_cli_evidence_recovery`); e == nil {
		t.Fatal("archive deletion allowed")
	}
}
func TestCLIRecoveryArchiveCorruptionRefused(t *testing.T) {
	for _, kind := range []string{"missing-trigger", "bad-fingerprint", "missing-archive", "bad-source", "missing-annotation"} {
		t.Run(kind, func(t *testing.T) {
			f := newRestoreFixture(t)
			seedRecoveredCLI(t, f)
			b := restoreBundle(t, f)
			switch kind {
			case "missing-trigger":
				mutateBundleSQL(t, b.Path, `DROP TRIGGER review_cli_recovery_immutable`)
			case "bad-fingerprint":
				mutateBundleSQL(t, b.Path, `DROP TRIGGER review_cli_recovery_immutable; UPDATE review_cli_evidence_recovery SET original_fingerprint=replace(original_fingerprint,substr(original_fingerprint,1,1),'z'); CREATE TRIGGER review_cli_recovery_immutable BEFORE UPDATE ON review_cli_evidence_recovery BEGIN SELECT RAISE(ABORT,'retain CLI recovery evidence'); END`)
			case "missing-archive":
				mutateBundleSQL(t, b.Path, `DROP TRIGGER review_cli_recovery_retain; DELETE FROM review_cli_evidence_recovery; CREATE TRIGGER review_cli_recovery_retain BEFORE DELETE ON review_cli_evidence_recovery BEGIN SELECT RAISE(ABORT,'retain CLI recovery evidence'); END`)
			case "bad-source":
				mutateBundleSQL(t, b.Path, `DROP TRIGGER review_cli_recovery_immutable; UPDATE review_cli_evidence_recovery SET original_body_json=json_set(original_body_json,'$.capture.capture_bytes',9); CREATE TRIGGER review_cli_recovery_immutable BEFORE UPDATE ON review_cli_evidence_recovery BEGIN SELECT RAISE(ABORT,'retain CLI recovery evidence'); END`)
			case "missing-annotation":
				mutateBundleSQL(t, b.Path, `UPDATE review_execution_evidence SET body_json=json_remove(body_json,'$.recovery')`)
			}
			if _, e := Inspect(t.Context(), b.Path); e == nil {
				t.Fatal("accepted corruption", kind)
			}
		})
	}
}
