package reviewexec

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestFrontsHTTPReadScopeSelectorsAndNoDispatch(t *testing.T) {
	f := newHTTPFixture(t)
	path := "/api/v1/projects/" + attemptProject + "/reviews/fronts"
	before := selectionCounts(t, &attemptFixture{httpFixture: f})
	for _, token := range []string{f.reader, f.writer, f.admin} {
		response := f.request(http.MethodGet, path, token, "", "")
		var value FrontSnapshot
		if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &value) != nil || value.ProjectID != attemptProject || value.Configured || value.RepositoryID != 801 || value.ConfigurationRevision != 0 || value.Queue == nil || value.Channels == nil || len(value.Queue) != 0 || len(value.Channels) != 0 {
			t.Fatalf("invalid unconfigured front read: %d %s", response.Code, response.Body.String())
		}
	}
	for _, token := range []string{"", f.other} {
		response := f.request(http.MethodGet, path, token, "", "")
		if response.Code != http.StatusUnauthorized && response.Code != http.StatusForbidden {
			t.Fatal("front scope admitted", response.Code)
		}
	}
	for _, query := range []string{"?snapshot=old", "?limit=100", "?refresh=true", "?channel=cli&channel=hosted", "?%zz"} {
		response := f.request(http.MethodGet, path+query, f.reader, "", "")
		if response.Code != http.StatusBadRequest {
			t.Fatal("unsupported front selector accepted", query, response.Code)
		}
	}
	if before != selectionCounts(t, &attemptFixture{httpFixture: f}) {
		t.Fatal("front read changed selection/operation/event/receipt counts")
	}
}

func TestFrontsHTTPCorruptSourceObligationReturnsDiagnosticHold(t *testing.T) {
	f := frontFixture(t)
	reader := &refreshTestReader{}
	frontSelect(t, f, "front-before-source-corruption", "cli", reader)
	retainObligationSource(t, f, attemptProject, "front-source-importer", "front-source", strings.Repeat("c", 64), 0)
	path := "/api/v1/projects/" + attemptProject + "/reviews/fronts"
	for _, corrupt := range []bool{false, true} {
		if corrupt {
			admissionSQL(t, f, `UPDATE review_adoption_obligations SET revision=2 WHERE project_id=?`, attemptProject)
		}
		before := selectionCounts(t, f)
		response := f.request(http.MethodGet, path, f.reader, "", "")
		var value FrontSnapshot
		if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &value) != nil {
			t.Fatalf("front read failed instead of retaining diagnostic: status=%d", response.Code)
		}
		reason := "source_obligations_pending"
		if corrupt {
			reason = "source_obligations_unavailable"
		}
		got := frontChannel(t, value, "cli")
		if got.State != "held" || got.Reason != reason || got.PullNumber != 2 {
			t.Fatal("incorrect source hold diagnostic", got)
		}
		if before != selectionCounts(t, f) || reader.calls.Load() != 2 {
			t.Fatal("local front read changed state or fetched provider facts")
		}
	}
}
