package reviews

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

func subagentFixture(t *testing.T) (*apiFixture, string, string) {
	t.Helper()
	f := newAPIFixture(t)
	p := samplePull(7, "Independent review target")
	p.State = "open"
	p.Complete = false
	p.HeadSHA = strings.Repeat("a", 40)
	f.observe(t, reviewRequest(0, []PullInput{p}), "subagent-seed")
	return f, subagentWriter(t, f), "/api/v1/projects/" + f.project + "/reviews/subagent-reports"
}
func subagentWriter(t *testing.T, f *apiFixture) string {
	t.Helper()
	token := "subagent-writer-token-00000000000000000"
	sum := sha256.Sum256([]byte(token))
	if _, err := f.store.DB().Exec(`INSERT INTO credentials(id,principal_id,token_sha256,is_admin,revoked) VALUES('subagent-writer','subagent-principal',?,0,0)`, hex.EncodeToString(sum[:])); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.DB().Exec(`INSERT INTO grants(credential_id,project_id,can_read,can_write) VALUES('subagent-writer',?,1,1)`, f.project); err != nil {
		t.Fatal(err)
	}
	return token
}
func subagentInput() SubagentRecordRequest {
	return SubagentRecordRequest{PullNumber: 7, HeadSHA: strings.Repeat("b", 40), Reviewer: "Independent agent", Scope: "Database invariants", StartedAt: "2026-01-01T01:00:00Z", FinishedAt: "2026-01-01T01:02:00Z", Findings: []SubagentFindingInput{{Title: "Retain owner identity", Detail: "**Proof** in `source.go:12`.", Severity: "high"}}}
}
func subagentRecord(t *testing.T, f *apiFixture, token, base, key string, in SubagentRecordRequest) SubagentReceipt {
	t.Helper()
	w := f.call(http.MethodPost, base, token, key, in)
	var out SubagentReceipt
	if w.Code != 201 || json.Unmarshal(w.Body.Bytes(), &out) != nil || out.State != "recorded" || !subagentRunID.MatchString(out.ID) {
		t.Fatal("record", w.Code, w.Body.String())
	}
	return out
}
func subagentRead(t *testing.T, f *apiFixture, token, path string) SubagentReport {
	t.Helper()
	w := f.call(http.MethodGet, path, token, "", nil)
	var out SubagentReport
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil {
		t.Fatal("read", w.Code, w.Body.String())
	}
	return out
}
func TestSubagentWorkerLifecycleStableRevisionAndFrozenCounts(t *testing.T) {
	f, token, base := subagentFixture(t)
	in := subagentInput()
	first := subagentRecord(t, f, token, base, "record-once", in)
	replay := subagentRecord(t, f, token, base, "record-once", in)
	if first != replay {
		t.Fatal("replay duplicated")
	}
	state := subagentRead(t, f, f.readToken, base+"/"+first.ID)
	if state.Revision != first.Revision || state.Run.Source != subagentSource || state.Run.Channel != "subagent" || state.Run.HeadSHA != in.HeadSHA || state.Run.Finalized || state.Run.Found != 1 || state.Findings[0].Detail != in.Findings[0].Detail {
		t.Fatal(state)
	}
	oldSnapshot := f.call("GET", "/api/v1/projects/"+f.project+"/reviews/runs/"+first.ID, f.readToken, "", nil)
	initialVersion := state.Revision
	// This independent append advances workspace only, leaving report CAS stable.
	other := in
	other.Findings = []SubagentFindingInput{}
	other.Finalized = true
	subagentRecord(t, f, token, base, "other-report", other)
	if subagentRead(t, f, f.readToken, base+"/"+first.ID).Revision != initialVersion {
		t.Fatal("unrelated append changed CAS")
	}
	request := SubagentUpdateRequest{ExpectedRevision: &initialVersion, Decisions: []SubagentDecisionInput{{Finding: 0, Disposition: "accepted", Reason: "Confirmed against source"}}, Finalize: true}
	w := f.call("POST", base+"/"+first.ID+"/updates", token, "decide-finalize", request)
	var updated SubagentReceipt
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &updated) != nil {
		t.Fatal(w.Code, w.Body.String())
	}
	now := subagentRead(t, f, f.readToken, base+"/"+first.ID)
	if !now.Run.Finalized || now.Run.Accepted != 1 || now.Findings[0].FixSHA != "" {
		t.Fatal(now)
	}
	empty := ""
	fix := SubagentUpdateRequest{ExpectedRevision: &now.Revision, Fixes: []SubagentFixInput{{Finding: 0, ExpectedSHA: &empty, FixSHA: strings.Repeat("c", 40), ProofNote: "Focused test passes"}}}
	w = f.call("POST", base+"/"+first.ID+"/updates", token, "fix", fix)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	current := subagentRead(t, f, f.readToken, base+"/"+first.ID)
	if current.Run.Found != 1 || current.Run.Accepted != 1 || current.Run.Fixed != 1 || current.Run.HeadSHA != in.HeadSHA || current.Findings[0].Title != in.Findings[0].Title {
		t.Fatal(current)
	}
	if w = f.call("POST", base+"/"+first.ID+"/updates", token, "fix", fix); w.Code != 200 {
		t.Fatal("same-key recovery failed")
	}
	// Original pinned history remains the original unadjudicated report.
	var detail RunDetailResponse
	_ = json.Unmarshal(oldSnapshot.Body.Bytes(), &detail)
	old := f.call("GET", "/api/v1/projects/"+f.project+"/reviews/runs/"+first.ID+"?snapshot="+detail.SnapshotID, f.readToken, "", nil)
	if old.Code != 200 || !strings.Contains(old.Body.String(), `"disposition":"unadjudicated"`) {
		t.Fatal("historical source changed")
	}
}
func TestSubagentZeroAndAtomicValidationAuth(t *testing.T) {
	f, token, base := subagentFixture(t)
	zero := subagentInput()
	zero.Findings = []SubagentFindingInput{}
	zero.Finalized = true
	r := subagentRecord(t, f, token, base, "zero-review", zero)
	state := subagentRead(t, f, f.readToken, base+"/"+r.ID)
	if !state.Run.Finalized || state.Run.Found != 0 || len(state.Findings) != 0 {
		t.Fatal(state)
	}
	original := subagentInput()
	created := subagentRecord(t, f, token, base, "report", original)
	before := importDatabaseState(t, f)
	for _, body := range []string{`null`, `{}`, `{"pull_number":7,"head_sha":null}`, `{"pull_number":7,"head_sha":"` + strings.Repeat("b", 40) + `","reviewer":"x","scope":"x","started_at":"2026-01-01T00:00:00Z","finished_at":"2026-01-01T00:01:00Z","findings":null}`, `{"pull_number":7,"head_sha":"` + strings.Repeat("b", 40) + `","reviewer":"x","scope":"x","started_at":"2026-01-01T00:00:00Z","finished_at":"2026-01-01T00:01:00Z","findings":[{"title":"x","severity":"high"}]}`, `{"provider":"coderabbit"}`} {
		if w := f.call("POST", base, token, "invalid", json.RawMessage(body)); w.Code != 400 {
			t.Fatal("invalid accepted", w.Code)
		}
	}
	if w := f.call("POST", base, f.readToken, "readonly", original); w.Code != 403 {
		t.Fatal("reader wrote", w.Code)
	}
	req := SubagentUpdateRequest{ExpectedRevision: &created.Revision, Decisions: []SubagentDecisionInput{{Finding: 0, Disposition: "accepted", Reason: "verified"}, {Finding: 9, Disposition: "rejected", Reason: "bad index"}}}
	if w := f.call("POST", base+"/"+created.ID+"/updates", token, "bad-later", req); w.Code != 400 {
		t.Fatal(w.Code, w.Body.String())
	}
	if !reflect.DeepEqual(before, importDatabaseState(t, f)) {
		t.Fatal("invalid or failed batch left effects")
	}
}

func TestSubagentLiveProducerCannotBeForgedOrOverwrittenByImport(t *testing.T) {
	f, token, base := subagentFixture(t)
	in := subagentInput()
	created := subagentRecord(t, f, token, base, "live", in)
	state := subagentRead(t, f, f.readToken, base+"/"+created.ID)
	run := RunInput{SourceRunID: state.Run.SourceRunID, Channel: "subagent", HeadSHA: in.HeadSHA, Reviewer: in.Reviewer, Scope: in.Scope, Outcome: "completed", Attributable: true, Complete: true, StartedAt: in.StartedAt, FinishedAt: in.FinishedAt, Found: 1, Findings: []Finding{state.Findings[0].Finding}}
	pull := samplePull(7, "Independent review target")
	pull.State = "open"
	pull.HeadSHA = strings.Repeat("a", 40)
	pull.Complete = false
	pull.Runs = []RunInput{run}
	before := importDatabaseState(t, f)
	for _, forged := range []bool{false, true} {
		changed := pull
		changed.Runs = append([]RunInput(nil), pull.Runs...)
		changed.Runs[0].Findings = append([]Finding(nil), run.Findings...)
		if forged {
			changed.Runs[0].SourceRunID = "forged-live-report"
		} else {
			changed.Runs[0].Findings[0].Detail = "rewritten unfinalized capture"
		}
		req := transferRequest(state.Revision, TransferGroup{Source: subagentSource, Pulls: []PullInput{changed}})
		preview := f.call("POST", "/api/v1/projects/"+f.project+"/reviews/transfer/preview", f.admin, "", req)
		if preview.Code != 409 {
			t.Fatal("history transfer altered live producer", preview.Code, preview.Body.String())
		}
		obs := reviewRequest(state.Revision, []PullInput{changed})
		obs.Source = subagentSource
		if w := f.call("POST", "/api/v1/projects/"+f.project+"/reviews/observations", f.admin, "forged-observation", obs); w.Code != 409 {
			t.Fatal("observation altered live producer", w.Code, w.Body.String())
		}
	}
	if !reflect.DeepEqual(before, importDatabaseState(t, f)) {
		t.Fatal("rejected imports left effects")
	}
	// A byte-equivalent retained copy remains usable for deliberate history transfer.
	transferPreviewFor(t, f, transferRequest(state.Revision, TransferGroup{Source: subagentSource, Pulls: []PullInput{pull}}))
}

func TestSubagentRecordPreservesCompletePRFacts(t *testing.T) {
	f, token, base := subagentFixture(t)
	p := samplePull(7, "Independent review target")
	p.State = "open"
	p.HeadSHA = strings.Repeat("a", 40)
	f.observe(t, reviewRequest(1, []PullInput{p}), "complete-pr-observation")
	subagentRecord(t, f, token, base, "independent-complete-pr", subagentInput())
	w := f.call("GET", "/api/v1/projects/"+f.project+"/reviews/pulls/7", f.readToken, "", nil)
	var out PullResponse
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil || !out.Pull.Complete || out.Pull.HeadSHA != p.HeadSHA || out.Pull.CIState != p.CIState {
		t.Fatal("report replaced PR facts", w.Code, w.Body.String())
	}
}
