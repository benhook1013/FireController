package reviewexec

import (
	"bytes"
	"encoding/json"
	"github.com/benhook1013/FireController/internal/reviewprovider"
	"os"
	"testing"
)

func TestOptionalRetainedCLICaptureRecovery(t *testing.T) {
	path := os.Getenv("FC_DIAG_CAPTURE")
	if path == "" {
		t.Skip("optional private retained capture not supplied")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var d struct {
		Capture struct {
			Stdout   []byte `json:"stdout"`
			ExitCode int    `json:"exit_code"`
		} `json:"capture"`
	}
	if err = json.Unmarshal(b, &d); err != nil {
		t.Fatal(err)
	}
	var c map[string]any
	json.Unmarshal(bytes.Split(d.Capture.Stdout, []byte("\n"))[0], &c)
	parsed, err := reviewprovider.ParseCLIReview(d.Capture.Stdout, d.Capture.ExitCode, reviewprovider.CLIContext{BaseBranch: c["baseBranch"].(string), WorkingDirectory: c["workingDirectory"].(string)})
	if err != nil || !parsed.Complete || len(parsed.ReviewedFiles) == 0 {
		t.Fatal("original capture failed", err)
	}
	f, a, old, in := invalidProjectionFixture(t)
	in.Review = parsed
	if _, err = f.e.PublishEvidence(t.Context(), a.ProjectID, old.ID, cliPublicationPull(2)); err != nil {
		t.Fatal(err)
	}
	if _, err = f.e.RecoverInvalidCLIProjection(t.Context(), a.ProjectID, a.ID, a.Revision, in); err != nil {
		t.Fatal(err)
	}
	if err = f.e.RecoverRetainedCLI(t.Context(), "test-capability"); err != nil {
		t.Fatal(err)
	}
	got, err := f.e.ReadReviewEvidence(t.Context(), evidenceReadScope(t, f, a.ProjectID, f.reader), old.ID)
	if err != nil || encoded(got.Capture.Review) != encoded(parsed) {
		t.Fatal("findings changed", err)
	}
	t.Log("Original provider output accepted without edits; all findings and file inventory retained through recovered ledger and native publication; no provider requests")
}
