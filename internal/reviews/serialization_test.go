package reviews

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/core"
)

func TestReviewRunSerializedResponseIncludesNewlineInBudget(t *testing.T) {
	const budget = 4 << 20
	f := newAPIFixture(t)
	input := RunInput{SourceRunID: "serialization-source", Channel: "manual", Scope: "synthetic", Outcome: "completed", Complete: true, StartedAt: "2026-01-01T00:00:00Z", FinishedAt: "2026-01-01T00:01:00Z", Found: 100, Findings: []Finding{}}
	for i := 0; i < 100; i++ {
		input.Findings = append(input.Findings, Finding{SourceFindingID: fmt.Sprintf("finding-%03d", i), Title: "Literal <tag> & Ω", Severity: "unknown", Disposition: "unadjudicated"})
	}
	pull := samplePull(1, "Serialization fixture")
	pull.Runs = []RunInput{input}
	f.observe(t, reviewRequest(0, []PullInput{pull}), "serialization-import")
	base := "/api/v1/projects/" + f.project + "/reviews"
	page := f.call(http.MethodGet, base+"/pulls/1/runs", f.readToken, "", nil)
	var runs RunsResponse
	if page.Code != 200 || json.Unmarshal(page.Body.Bytes(), &runs) != nil || len(runs.Items) != 1 {
		t.Fatal("missing public run fixture")
	}
	path := base + "/runs/" + runs.Items[0].ID
	initial := f.call(http.MethodGet, path, f.readToken, "", nil)
	var expected RunDetailResponse
	if initial.Code != 200 || json.Unmarshal(initial.Body.Bytes(), &expected) != nil {
		t.Fatal("cannot read public detail fixture")
	}
	if initial.Header().Get("X-Request-ID") == "" || initial.Header().Get("Content-Type") != "application/json" || initial.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("initial response omitted generated identity or JSON privacy headers")
	}
	encoded, err := json.Marshal(expected)
	if err != nil {
		t.Fatal(err)
	}
	remaining := budget - 1 - len(encoded)
	for i := range expected.Findings {
		count := min(remaining/6, 16<<10)
		expected.Findings[i].Detail = strings.Repeat("<", count)
		remaining -= 6 * count
		if remaining < 6 && len(expected.Findings[i].Detail)+remaining <= 16<<10 {
			expected.Findings[i].Detail += strings.Repeat("x", remaining)
			remaining = 0
		}
	}
	if remaining != 0 {
		t.Fatal("fixture cannot reach the full response boundary")
	}
	// Disposable SQL setup bypasses only the smaller observation-body budget.
	// Each retained source finding still respects its individual 16 KiB limit.
	tx, err := f.store.DB().Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for _, finding := range expected.Findings {
		if len(finding.Detail) > 16<<10 {
			t.Fatal("source fixture exceeds finding limit")
		}
		if _, err := tx.Exec(`UPDATE review_findings SET detail=? WHERE project_id=? AND snapshot_id=? AND run_id=? AND source_finding_id=?`, finding.Detail, f.project, expected.SnapshotID, expected.Run.ID, finding.SourceFindingID); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	counts := func() [3]int {
		var result [3]int
		for i, table := range []string{"events", "receipts", "review_snapshots"} {
			if err := f.store.DB().QueryRow("SELECT count(*) FROM " + table).Scan(&result[i]); err != nil {
				t.Fatal(err)
			}
		}
		return result
	}
	before := counts()
	requestID := core.NewID("request")
	read := func() *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		r.Header.Set("Authorization", "Bearer "+f.readToken)
		w := httptest.NewRecorder()
		w.Header().Set("X-Request-ID", requestID)
		f.mux.ServeHTTP(w, r)
		return w
	}
	encoded, err = json.Marshal(expected)
	if err != nil || len(encoded)+1 != budget {
		t.Fatalf("bad fixture wire size %d: %v", len(encoded)+1, err)
	}
	accepted := read()
	if accepted.Code != 200 || accepted.Header().Get("Content-Type") != "application/json" || accepted.Header().Get("Cache-Control") != "no-store" || accepted.Header().Get("X-Request-ID") != requestID {
		t.Fatal("successful detail response changed status or headers", accepted.Code, accepted.Header())
	}
	if !bytes.Equal(accepted.Body.Bytes(), append(encoded, '\n')) || accepted.Body.Len() != budget {
		t.Fatalf("validated serialized bytes changed or were truncated: %d", accepted.Body.Len())
	}
	if !bytes.Contains(accepted.Body.Bytes(), []byte(`\u003c`)) || bytes.Contains(accepted.Body.Bytes(), []byte("<")) {
		t.Fatal("large source text was not safely JSON encoded")
	}
	var decoded RunDetailResponse
	if err := json.Unmarshal(accepted.Body.Bytes(), &decoded); err != nil || !reflect.DeepEqual(decoded, expected) {
		t.Fatal("boundary response did not preserve complete source finding text", err)
	}
	// The complete JSON object fits without its newline, but the actual response
	// is one byte over budget. It must be rejected before any detail is written.
	index := -1
	for i, finding := range expected.Findings {
		if len(finding.Detail) < (16<<10)-1 {
			index = i
			break
		}
	}
	if index < 0 {
		t.Fatal("no bounded fixture field has room")
	}
	for _, extra := range []string{"x", "<"} {
		expected.Findings[index].Detail += extra
		if _, err := f.store.DB().Exec(`UPDATE review_findings SET detail=? WHERE project_id=? AND snapshot_id=? AND run_id=? AND source_finding_id=?`, expected.Findings[index].Detail, f.project, expected.SnapshotID, expected.Run.ID, expected.Findings[index].SourceFindingID); err != nil {
			t.Fatal(err)
		}
		rejected := read()
		var failure struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		if rejected.Code != 500 || json.Unmarshal(rejected.Body.Bytes(), &failure) != nil || failure.Error.Code != "response_too_large" || rejected.Body.Len() > 1024 || bytes.Contains(rejected.Body.Bytes(), []byte("finding-000")) {
			t.Fatalf("oversized response was written or truncated instead of safely rejected: status=%d bytes=%d", rejected.Code, rejected.Body.Len())
		}
		if rejected.Header().Get("Content-Type") != "application/json" || rejected.Header().Get("Cache-Control") != "no-store" || rejected.Header().Get("X-Request-ID") != requestID {
			t.Fatal("error response headers changed")
		}
	}
	if counts() != before {
		t.Fatal("detail reads produced durable effects")
	}
}
