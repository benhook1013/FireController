package cli_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/adoptionholdsmodel"
	"github.com/benhook1013/FireController/internal/cli"
)

func obligationBatch() adoptionholdsmodel.Batch {
	zero := int64(0)
	return adoptionholdsmodel.Batch{Version: 1, Source: adoptionholdsmodel.Source{Namespace: "sealed-source", RepositoryID: 7, SnapshotSHA256: strings.Repeat("a", 64)}, ExpectedRevision: &zero, Obligations: []adoptionholdsmodel.Obligation{{SourceKey: "finding-one", Kind: "finding", AffectedPullNumbers: []int64{7}, Summary: "retain correction", Evidence: []string{}, OriginalStatus: "open"}}}
}

func TestAdoptionObligationSyntaxAndPrivateBatchFailBeforeProfile(t *testing.T) {
	for _, args := range [][]string{
		{"reviews", "obligations", "list", "--status", "all"},
		{"reviews", "obligations", "read", "bad"},
		{"reviews", "obligations", "adopt", "preview", "--file", "relative"},
		{"reviews", "obligations", "adopt", "apply", "--file", "/missing"},
		{"reviews", "obligations", "resolve", "adoption_obligation_00000000000000000000000000000000", "--expected-revision", "1", "--reason", "done", "--key", "same-key"},
	} {
		code, output, diagnostic := run(append([]string{"--profiles-file", "/missing/profile"}, args...), nil)
		if code != cli.ExitUsage || output != "" || !strings.Contains(diagnostic, "invalid_arguments") {
			t.Fatal(args, code, diagnostic)
		}
	}
	directory := t.TempDir()
	file := filepath.Join(directory, "obligations.json")
	valid, _ := json.Marshal(obligationBatch())
	for _, raw := range [][]byte{[]byte("null"), []byte(`{"version":1}`), append(valid, []byte(`{"extra":true}`)...)} {
		if err := os.WriteFile(file, raw, 0600); err != nil {
			t.Fatal(err)
		}
		code, output, diagnostic := run([]string{"--profiles-file", "/missing/profile", "reviews", "obligations", "adopt", "preview", "--file", file}, nil)
		if code == 0 || output != "" || strings.Contains(diagnostic, "profile") {
			t.Fatal("invalid batch reached profile loading", code, diagnostic)
		}
	}
	if runtime.GOOS != "windows" {
		if err := os.WriteFile(file, valid, 0644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(file, 0644); err != nil {
			t.Fatal(err)
		}
		code, _, diagnostic := run([]string{"reviews", "obligations", "adopt", "preview", "--file", file}, nil)
		if code == 0 || !strings.Contains(diagnostic, "invalid_import_file") {
			t.Fatal("non-private adoption file admitted", code, diagnostic)
		}
	}
}

func TestAdoptionObligationResolveRequiresEvidenceBeforeProfile(t *testing.T) {
	id := "adoption_obligation_00000000000000000000000000000000"
	base := []string{"--profiles-file", "/missing/profile", "reviews", "obligations", "resolve", id, "--expected-revision", "1", "--reason", "addressed", "--key", "resolution-key"}
	code, _, diagnostic := run(base, nil)
	if code != cli.ExitUsage || !strings.Contains(diagnostic, "invalid_arguments") {
		t.Fatal("empty resolution evidence admitted", code, diagnostic)
	}
	code, _, diagnostic = run(append(base, "--evidence", "native:proof"), nil)
	if code != cli.ExitConfig || !strings.Contains(diagnostic, "profile") {
		t.Fatal("valid resolution did not reach profile boundary", code, diagnostic)
	}
}
