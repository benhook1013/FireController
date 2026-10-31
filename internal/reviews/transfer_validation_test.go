package reviews

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/core"
)

func validationTransfer() TransferRequest {
	revision := int64(0)
	return TransferRequest{Version: 1, ExpectedRevision: &revision, RepositoryID: 7, Publisher: "export-publisher", Groups: []TransferGroup{{Source: "original-a", Pulls: []PullInput{{Number: 1, Title: "Selected project work", State: "merged", CIState: "unknown", Mergeability: "unknown", Runs: []RunInput{{SourceRunID: "same-run", Channel: "cli", Scope: "full", Outcome: "unknown"}}, Attempts: []Attempt{{SourceAttemptID: "same-attempt", Channel: "cli", State: "unknown", SourceRunID: "same-run"}}}}}}}
}

func TestTransferNormalizationPreservesProducersAndUnknownTimes(t *testing.T) {
	req := validationTransfer()
	second := req.Groups[0]
	second.Source = "original-b"
	req.Groups = append(req.Groups, second)
	body, err := normalizeTransfer(&req)
	if err != nil {
		t.Fatal(err)
	}
	if req.Groups[0].Pulls[0].Runs[0].StartedAt != "" || req.Groups[0].Pulls[0].Runs[0].FinishedAt != "" {
		t.Fatal("invented historical times")
	}
	if bytes.Contains(body, []byte(`"findings":null`)) || bytes.Contains(body, []byte(`"coverage_limits":null`)) {
		t.Fatal("nil nested collections not normalized")
	}
	scope := core.Scope{ProjectID: "project", PrincipalID: "admin", WorkerID: "worker"}
	preview, err := transferPreview("instance", scope, req, body)
	if err != nil || preview.Counts != (TransferCounts{Pulls: 1, Runs: 2, Attempts: 2}) || preview.Coverage != "partial" || !preview.ImportedHistory || preview.NativeCoverage || len(preview.PreviewSHA256) != 64 {
		t.Fatal(preview, err)
	}
	normalized, err := normalizeTransfer(&req)
	if err != nil || !bytes.Equal(body, normalized) {
		t.Fatal("normalization not stable", err)
	}
	raw, _ := json.Marshal(preview)
	if bytes.Contains(raw, []byte("Selected project work")) || bytes.Contains(raw, []byte("same-run")) || bytes.Contains(raw, []byte("original-a")) {
		t.Fatal("preview echoed private records")
	}
	for _, change := range []func(*core.Scope, *string){
		func(s *core.Scope, _ *string) { s.ProjectID = "other" }, func(s *core.Scope, _ *string) { s.PrincipalID = "other" }, func(s *core.Scope, _ *string) { s.WorkerID = "other" }, func(_ *core.Scope, i *string) { *i = "other" },
	} {
		changed, instance := scope, "instance"
		change(&changed, &instance)
		got, err := transferPreview(instance, changed, req, body)
		if err != nil || got.PreviewSHA256 == preview.PreviewSHA256 || got.PayloadSHA256 != preview.PayloadSHA256 {
			t.Fatal("destination binding failed", got, err)
		}
	}
	req.Publisher = "another-publisher"
	changedBody, err := normalizeTransfer(&req)
	if err != nil {
		t.Fatal(err)
	}
	changed, err := transferPreview("instance", scope, req, changedBody)
	if err != nil || changed.PreviewSHA256 == preview.PreviewSHA256 || changed.PayloadSHA256 == preview.PayloadSHA256 {
		t.Fatal("content not bound", err)
	}
}

func TestTransferLinksDeferOnlyDestinationChecks(t *testing.T) {
	req := validationTransfer()
	req.Groups[0].Pulls[0].Attempts[0].SourceRunID = "retained-run"
	if _, err := normalizeTransfer(&req); err != nil {
		t.Fatal("retained link rejected before destination admission", err)
	}
	observation := ObservationRequest{ExpectedRevision: req.ExpectedRevision, RepositoryID: req.RepositoryID, Source: "original-a", ObservedAt: "2026-01-01T00:00:00Z", Queue: req.Groups[0].Pulls}
	if err := validateObservation(&observation); err == nil {
		t.Fatal("complete observation accepted omitted linked run")
	}
	req.Groups[0].Pulls[0].Attempts[0].SourceRunID = strings.Repeat("x", 257)
	if _, err := normalizeTransfer(&req); err == nil {
		t.Fatal("invalid link shape escaped common validation")
	}
}

func TestTransferValidationRejectsInvalidBatches(t *testing.T) {
	tests := map[string]func(*TransferRequest){
		"version": func(r *TransferRequest) { r.Version = 2 }, "revision missing": func(r *TransferRequest) { r.ExpectedRevision = nil }, "revision negative": func(r *TransferRequest) { *r.ExpectedRevision = -1 }, "repository": func(r *TransferRequest) { r.RepositoryID = 0 }, "publisher": func(r *TransferRequest) { r.Publisher = " " }, "empty groups": func(r *TransferRequest) { r.Groups = nil }, "empty group": func(r *TransferRequest) { r.Groups[0].Pulls = nil }, "duplicate source": func(r *TransferRequest) { r.Groups = append(r.Groups, r.Groups[0]) }, "source": func(r *TransferRequest) { r.Groups[0].Source = " " }, "complete": func(r *TransferRequest) { r.Groups[0].Pulls[0].Complete = true }, "duplicate pull": func(r *TransferRequest) { r.Groups[0].Pulls = append(r.Groups[0].Pulls, r.Groups[0].Pulls[0]) }, "duplicate run": func(r *TransferRequest) {
			p := r.Groups[0].Pulls[0]
			p.Number = 2
			r.Groups[0].Pulls = append(r.Groups[0].Pulls, p)
		}, "duplicate attempt": func(r *TransferRequest) {
			p := r.Groups[0].Pulls[0]
			p.Number = 2
			p.Runs = nil
			r.Groups[0].Pulls = append(r.Groups[0].Pulls, p)
		}, "metadata conflict": func(r *TransferRequest) {
			p := r.Groups[0].Pulls[0]
			p.Title = "different"
			r.Groups = append(r.Groups, TransferGroup{Source: "b", Pulls: []PullInput{p}})
		}, "invalid count": func(r *TransferRequest) { r.Groups[0].Pulls[0].Runs[0].Accepted = 1 }, "invalid time": func(r *TransferRequest) { r.Groups[0].Pulls[0].Runs[0].Outcome = "completed" }, "finding identity": func(r *TransferRequest) {
			run := &r.Groups[0].Pulls[0].Runs[0]
			run.Found = 1
			run.Findings = []Finding{{Title: "Missing identity", Severity: "unknown", Disposition: "unadjudicated"}}
		},
	}
	for name, change := range tests {
		t.Run(name, func(t *testing.T) {
			req := validationTransfer()
			change(&req)
			if _, err := normalizeTransfer(&req); err == nil {
				t.Fatal("invalid transfer admitted")
			}
		})
	}
}

func TestTransferAggregateBoundsAcrossProducers(t *testing.T) {
	makeBatch := func(groups, runs, attempts, findings int) TransferRequest {
		r := validationTransfer()
		r.Groups = nil
		for g := 0; g < groups; g++ {
			p := validationTransfer().Groups[0].Pulls[0]
			p.Runs = nil
			p.Attempts = nil
			for i := 0; i < runs; i++ {
				run := RunInput{SourceRunID: fmt.Sprint("run-", i), Channel: "cli", Scope: "full", Outcome: "unknown", Found: int64(findings)}
				for f := 0; f < findings; f++ {
					run.Findings = append(run.Findings, Finding{SourceFindingID: fmt.Sprint("finding-", f), Title: "Finding", Severity: "unknown", Disposition: "unadjudicated"})
				}
				p.Runs = append(p.Runs, run)
			}
			for i := 0; i < attempts; i++ {
				p.Attempts = append(p.Attempts, Attempt{SourceAttemptID: fmt.Sprint("attempt-", i), Channel: "cli", State: "unknown"})
			}
			r.Groups = append(r.Groups, TransferGroup{Source: fmt.Sprint("source-", g), Pulls: []PullInput{p}})
		}
		return r
	}
	for _, tc := range []struct {
		name                             string
		groups, runs, attempts, findings int
		valid                            bool
	}{{"200 runs", 2, 100, 0, 0, true}, {"201 plus runs", 3, 67, 0, 0, false}, {"200 attempts", 2, 0, 100, 0, true}, {"201 plus attempts", 3, 0, 67, 0, false}, {"1000 findings", 10, 1, 0, 100, true}, {"findings aggregate", 11, 1, 0, 100, false}, {"100 appearances", 100, 0, 0, 0, true}, {"101 groups appearances", 101, 0, 0, 0, false}} {
		t.Run(tc.name, func(t *testing.T) {
			req := makeBatch(tc.groups, tc.runs, tc.attempts, tc.findings)
			_, err := normalizeTransfer(&req)
			if (err == nil) != tc.valid {
				t.Fatal("unexpected batch admission", err)
			}
		})
	}
	// Two valid producer groups can still exceed the batch appearance bound.
	appearances := makeBatch(2, 0, 0, 0)
	for i := int64(2); i <= 100; i++ {
		pull := appearances.Groups[0].Pulls[0]
		pull.Number = i
		appearances.Groups[0].Pulls = append(appearances.Groups[0].Pulls, pull)
	}
	if _, err := normalizeTransfer(&appearances); err == nil {
		t.Fatal("101 pull appearances admitted across individually valid groups")
	}
	// Each group remains individually bounded even when batch totals fit.
	for _, tc := range []struct{ runs, attempts, findings int }{{101, 0, 0}, {0, 101, 0}, {1, 0, 101}} {
		req := makeBatch(1, tc.runs, tc.attempts, tc.findings)
		if _, err := normalizeTransfer(&req); err == nil {
			t.Fatal("per-record bound removed")
		}
	}
}

func TestTransferNormalizedApplyWrapperBoundary(t *testing.T) {
	makeSized := func(total int) TransferRequest {
		r := validationTransfer()
		p := &r.Groups[0].Pulls[0]
		p.Runs[0].Found = 17
		for i := 0; i < 17; i++ {
			n := min(total, 16<<10)
			total -= n
			p.Runs[0].Findings = append(p.Runs[0].Findings, Finding{SourceFindingID: fmt.Sprint("f-", i), Title: "Finding", Detail: strings.Repeat("<", n), Severity: "unknown", Disposition: "unadjudicated"})
		}
		return r
	}
	lo, hi := 0, 17*(16<<10)
	for lo < hi {
		mid := (lo + hi + 1) / 2
		req := makeSized(mid)
		if _, err := normalizeTransfer(&req); err == nil {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	valid := makeSized(lo)
	body, err := normalizeTransfer(&valid)
	if err != nil {
		t.Fatal(err)
	}
	wrapper, _ := json.Marshal(TransferApplyRequest{PreviewSHA256: strings.Repeat("0", 64), Batch: valid})
	if len(wrapper) > maxObservationBytes || len(wrapper) < maxObservationBytes-6 {
		t.Fatal("not near wrapper boundary", len(wrapper))
	}
	invalid := makeSized(lo + 1)
	if _, err := normalizeTransfer(&invalid); err == nil {
		t.Fatal("unusable preview admitted")
	}
	raw, _ := json.Marshal(invalid)
	if len(raw) > maxObservationBytes {
		t.Fatal("test did not isolate future wrapper budget", len(body), len(raw))
	}
}
