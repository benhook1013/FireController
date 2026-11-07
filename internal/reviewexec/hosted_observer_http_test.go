package reviewexec

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHostedObserverReadNegotiationDoesNotChangeJournal(t *testing.T) {
	f, a, _ := evidenceFixture(t, "hosted", true)
	before := evidenceState(t, f, a)
	for _, compact := range []bool{false, true} {
		for _, header := range []string{"", "1"} {
			r := httptest.NewRequest("GET", "/api/v1/projects/"+attemptProject+"/reviews/operations/"+a.OperationID, nil)
			r.SetPathValue("project_id", attemptProject)
			r.SetPathValue("operation_id", a.OperationID)
			r.Header.Set("Authorization", "Bearer "+f.reader)
			r.Header.Set("X-FC-Hosted-Observer", header)
			w := httptest.NewRecorder()
			if compact {
				f.e.readOperationStatusHTTP(w, r)
			} else {
				f.e.readOperationHTTP(w, r)
			}
			if w.Code != 200 {
				t.Fatal(w.Code, w.Body.String())
			}
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(w.Body.Bytes(), &fields); err != nil {
				t.Fatal(err)
			}
			observer, present := fields["observer"]
			if present != (header == "1") || present && string(observer) != `{"state":"unknown"}` {
				t.Fatal("unobserved process must report unknown only to opted-in clients", w.Body.String())
			}
			if !strings.Contains(strings.Join(w.Header().Values("Vary"), ","), "X-FC-Hosted-Observer") {
				t.Fatal("observer negotiation missing Vary")
			}
		}
	}
	if before != evidenceState(t, f, a) {
		t.Fatal("observer read changed retained execution, receipts or evidence")
	}
}
