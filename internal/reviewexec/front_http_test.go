package reviewexec

import (
	"encoding/json"
	"net/http"
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
