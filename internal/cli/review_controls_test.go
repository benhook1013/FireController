package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/benhook1013/FireController/internal/cli"
	"github.com/benhook1013/FireController/internal/client"
)

const cliEvidenceID = "review_evidence_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func TestReviewControlCommandsExplicitReadsAndCompactMutationOutput(t *testing.T) {
	f := setupServiceFixture(t)
	project := f.project(t, "cli-controls")
	worker := f.worker(t, project, "Builder")
	var mutations atomic.Int64
	var badReceipt atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Header.Get("X-FC-Instance") != f.store.InstanceID() || r.Header.Get("Authorization") != "Bearer "+f.admin {
			t.Error("lost CLI instance/credential binding")
		}
		if r.URL.Path == "/api/v1/instance" {
			_ = json.NewEncoder(w).Encode(map[string]string{"instance_id": f.store.InstanceID()})
			return
		}
		if r.Header.Get("X-FC-Worker") != worker || !strings.HasPrefix(r.URL.Path, "/api/v1/projects/"+project+"/reviews/evidence/"+cliEvidenceID) {
			t.Error("lost CLI project/worker binding")
		}
		if r.Method == "GET" {
			if strings.HasSuffix(r.URL.Path, "/adjudication") {
				_ = json.NewEncoder(w).Encode(client.ReviewAdjudication{EvidenceID: cliEvidenceID, Findings: []client.ReviewFindingDecision{}})
				return
			}
			hash := strings.Repeat("a", 64)
			now := "2026-10-15T00:00:00Z"
			_ = json.NewEncoder(w).Encode(client.ReviewEvidence{ID: cliEvidenceID, ProjectID: project, AttemptID: "review_attempt_" + strings.Repeat("a", 32), OperationID: "review_operation_" + strings.Repeat("a", 32), CapabilityFingerprint: hash, Anchor: client.ReviewAnchor{RepositoryID: 1, Repository: "owner/repository", PullNumber: 1, CandidateSHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("b", 40), ParentSHA: strings.Repeat("b", 40), MergeBaseSHA: strings.Repeat("c", 40), PatchSHA256: hash, BaseRef: "main", ObservedAt: now}, Capture: client.ReviewEvidenceCapture{CaptureID: "review_capture_" + hash[:32], CaptureSHA256: hash, CaptureBytes: 1024, StartedAt: now, FinishedAt: now, Outcome: "completed", Complete: true, ReviewedFiles: []string{"source.go"}, Findings: []client.ReviewEvidenceFinding{}}, RecordedAt: now})
			return
		}
		mutations.Add(1)
		var request map[string]any
		if json.NewDecoder(r.Body).Decode(&request) != nil || request["expected_revision"] != float64(0) || r.Header.Get("Idempotency-Key") != "explicit-key" {
			t.Error("request lost explicit revision/key")
		}
		state := "finalized"
		if strings.HasSuffix(r.URL.Path, "/decisions") {
			state = "adjudicated"
			if request["finding"] != float64(0) || request["reason"] != "  exact private reason\n" {
				t.Error("decision index/text changed")
			}
		}
		if strings.HasSuffix(r.URL.Path, "/fixes") {
			state = "fix_recorded"
			expected, present := request["expected_sha"]
			if !present || expected != "" || request["finding"] != float64(0) || request["proof_note"] != "private proof" {
				t.Error("empty prior SHA/index/proof changed")
			}
		}
		revision := int64(1)
		if badReceipt.Load() {
			revision = 2
		}
		_ = json.NewEncoder(w).Encode(client.ReviewControlReceipt{ID: cliEvidenceID, State: state, Revision: revision})
	}))
	defer srv.Close()
	path := mutationProfiles(t, f, srv.URL, map[string][2]string{"builder": {project, worker}})
	for _, command := range []string{"evidence", "adjudication"} {
		code, out, stderr := mutationInvoke(t, path, "builder", "reviews", command, cliEvidenceID)
		if code != 0 || stderr != "" || !json.Valid([]byte(out)) {
			t.Fatalf("explicit read: %d %s %s", code, out, stderr)
		}
	}
	decide := []string{"reviews", "decide", cliEvidenceID, "--key", "explicit-key", "--expect-revision", "0", "--finding", "0", "--disposition", "accepted", "--reason", "  exact private reason\n"}
	fix := []string{"reviews", "fix", cliEvidenceID, "--key", "explicit-key", "--expect-revision=0", "--finding=0", "--expect-sha=", "--fix-sha", strings.Repeat("a", 40), "--proof", "private proof"}
	final := []string{"reviews", "finalize", cliEvidenceID, "--key", "explicit-key", "--expect-revision", "0"}
	for _, args := range [][]string{decide, fix, final} {
		receipt := mutationSuccess(t, path, "builder", args...)
		if receipt["revision"] != float64(1) || strings.Contains(encodeString(receipt), "private") {
			t.Fatal("mutation output was not a compact receipt")
		}
	}
	if mutations.Load() != 3 {
		t.Fatal("unexpected extra mutation requests")
	}
	var stderr bytes.Buffer
	args := append([]string{"--profiles-file", path}, decide...)
	code := cli.RunContext(context.Background(), args, map[string]string{"FC_PROFILE": "builder"}, failedMutationOutput{}, &stderr)
	if code != cli.ExitConfig || !strings.Contains(stderr.String(), `"outcome_unknown":true`) || !strings.Contains(stderr.String(), "same key") {
		t.Fatalf("stdout failure lost recovery guidance: %d %s", code, stderr.String())
	}
	badReceipt.Store(true)
	code, out, errOut := mutationInvoke(t, path, "builder", decide...)
	if code != cli.ExitService || out != "" || !strings.Contains(errOut, `"outcome_unknown":true`) || strings.Contains(errOut, "exact private reason") {
		t.Fatalf("invalid receipt lost safe uncertainty: %d %s %s", code, out, errOut)
	}
}

func encodeString(value any) string { data, _ := json.Marshal(value); return string(data) }

func TestReviewControlArgumentsFailBeforeProfileCredentialOrHTTP(t *testing.T) {
	decide := []string{"reviews", "decide", cliEvidenceID, "--key", "key", "--expect-revision", "0", "--finding", "0", "--disposition", "accepted", "--reason", "reason"}
	fix := []string{"reviews", "fix", cliEvidenceID, "--key", "key", "--expect-revision", "0", "--finding", "0", "--expect-sha", "", "--fix-sha", strings.Repeat("a", 40), "--proof", "proof"}
	without := func(args []string, flag string) []string {
		var result []string
		for i := 0; i < len(args); i++ {
			if args[i] == flag {
				i++
				continue
			}
			result = append(result, args[i])
		}
		return result
	}
	replace := func(args []string, flag, value string) []string {
		out := append([]string(nil), args...)
		for i := range out {
			if out[i] == flag {
				out[i+1] = value
			}
		}
		return out
	}
	invalid := [][]string{{"reviews", "evidence"}, {"reviews", "evidence", "../other"}, {"reviews", "evidence", cliEvidenceID, "--snapshot", "review_snapshot_" + strings.Repeat("a", 32)}, {"reviews", "adjudication", cliEvidenceID, "--limit", "1"}, without(decide, "--finding"), without(decide, "--expect-revision"), without(fix, "--expect-sha"), replace(decide, "--finding", "00"), replace(decide, "--finding", "100"), replace(decide, "--finding", "-1"), replace(decide, "--expect-revision", "+0"), replace(decide, "--expect-revision", "00"), replace(decide, "--expect-revision", "9223372036854775807"), replace(decide, "--disposition", "routed"), replace(decide, "--reason", "\x01"), replace(decide, "--reason", string([]byte{255})), replace(decide, "--key", " padded "), replace(fix, "--fix-sha", strings.Repeat("A", 40)), replace(fix, "--expect-sha", "bad"), append(append([]string(nil), decide...), "--finding", "0")}
	for _, args := range invalid {
		code, out, stderr := run(append([]string{"--profiles-file", "/unavailable/credential-and-profile"}, args...), map[string]string{"FC_PROFILE": "missing"})
		if code != cli.ExitUsage || out != "" || !strings.Contains(stderr, "invalid_arguments") {
			t.Fatalf("invalid control reached profile/credential instead of admission: %v: %d %s %s", args, code, out, stderr)
		}
	}
}
