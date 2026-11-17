package reviewexec

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/benhook1013/FireController/internal/reviewprovider"
)

func TestSourceCIDiagnosticsDistinguishNativeCheckAndSameNameStatus(t *testing.T) {
	f := newAttemptFixture(t)
	f.configure(t, attemptProject, "test-capability", 0, false)
	reader := &sourceCITestReader{ci: func(_ context.Context, target reviewprovider.CITarget) (reviewprovider.CIObservation, error) {
		return sourceCIAppStatusObservation(target, "pending", false), nil
	}}
	receipt := refreshEnqueue(t, f, "retained-contexts", sourceCIRequest())
	cancel, done := refreshStart(t, f, reader)
	refreshWait(t, f, receipt.ID, "completed")
	refreshStop(t, cancel, done)
	before := sourceCITestState(t, f)
	compact := readSourceCI(t, f)
	if compact.Checks != nil || compact.State != "pending" || compact.Required != 1 {
		t.Fatal("ordinary read grew an inventory", compact)
	}
	path := "/api/v1/projects/" + attemptProject + "/reviews/source/2/ci"
	for i := 0; i < 2; i++ {
		w := f.request("GET", path+"?diagnostics=true", f.reader, "", "")
		var v SourceCIAssessment
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &v) != nil || v.Checks == nil || len(*v.Checks) != 1 || v.Pending != 1 || v.RefreshID != receipt.ID || v.SelectedCommitSHA != compact.SelectedCommitSHA {
			t.Fatal("diagnostics lost retained identity or required counts", w.Code, w.Body.String())
		}
		c := (*v.Checks)[0]
		if c.Context != "build" || c.AppID != 15368 || c.State != "pending" || c.Reason != "ci_status_pending" || c.CheckRun == nil || c.CheckRun.ID != 300 || c.CheckRun.Status != "completed" || c.CheckRun.Conclusion != "success" || c.CommitStatus == nil || c.CommitStatus.ID != 991 || c.CommitStatus.State != "pending" {
			t.Fatal("same-name result kinds were conflated", c)
		}
		// Reconstructing the reader does not reinterpret the retained evidence.
		var err error
		f.e, err = New(f.store, attemptCapabilities)
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, suffix := range []string{"?diagnostics=1", "?diagnostics=", "?diagnostics=true&diagnostics=true", "?diagnostics=true&refresh=true"} {
		if w := f.request("GET", path+suffix, f.reader, "", ""); w.Code != 400 {
			t.Fatal("invalid diagnostic selector accepted", suffix, w.Code)
		}
	}
	var v SourceCIAssessment
	w := f.request("GET", path+"?diagnostics=false", f.reader, "", "")
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &v) != nil || v.Checks != nil {
		t.Fatal("explicit false expanded ordinary output")
	}
	if w := f.request("GET", path+"?diagnostics=true", f.other, "", ""); w.Code != 403 {
		t.Fatal("diagnostic read crossed project boundary", w.Code)
	}
	if sourceCITestState(t, f) != before || reader.ciCalls.Load() != 1 {
		t.Fatal("diagnostics mutated source or recollected GitHub")
	}
}

func TestSourceCIDiagnosticsRetainUnknownAndEmptyDistinction(t *testing.T) {
	for _, state := range []string{"not_required", "unproven"} {
		t.Run(state, func(t *testing.T) {
			f := newAttemptFixture(t)
			f.configure(t, attemptProject, "test-capability", 0, false)
			reader := &sourceCITestReader{ci: func(_ context.Context, target reviewprovider.CITarget) (reviewprovider.CIObservation, error) {
				o := sourceCITestObservation(target, "not_required")
				if state == "unproven" {
					o.State, o.Reason = "unproven", "ci_authority_unproven"
					o.Authority.State, o.Authority.Reason, o.Authority.Checks = "unproven", "unavailable", nil
					o.Assessment.State, o.Assessment.Reason = "unproven", "ci_authority_unproven"
				}
				return o, nil
			}}
			r := refreshEnqueue(t, f, "empty-contexts", sourceCIRequest())
			cancel, done := refreshStart(t, f, reader)
			refreshWait(t, f, r.ID, "completed")
			refreshStop(t, cancel, done)
			v, err := f.e.ReadSourceCIDiagnostics(t.Context(), sourceCIScope(t, f), 2)
			if err != nil || v.Checks == nil || *v.Checks == nil || len(*v.Checks) != 0 || v.State != state {
				t.Fatal("empty details changed evidence meaning", v, err)
			}
		})
	}
}
