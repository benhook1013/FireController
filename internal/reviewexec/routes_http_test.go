package reviewexec

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func TestFindingRouteHTTPStrictPublicIdentityAndGuardedWorkflow(t *testing.T) {
	f, a, evidence := routeFixture(t, 2)
	base := "/api/v1/projects/" + attemptProject + "/reviews"
	path := base + "/evidence/" + evidence + "/routes"
	request := `{"expected_revision":0,"finding":0,"target_pull_number":4,"reason":"Explicit source routing reason"}`
	before := routeState(t, f, a)
	for n, body := range []string{
		`{"expected_revision":0,"target_pull_number":4,"reason":"Missing finding"}`,
		strings.Replace(request, `"finding":0`, `"finding":null`, 1),
		strings.Replace(request, `"target_pull_number":4`, `"target_pull_number":null`, 1),
		strings.Replace(request, `"expected_revision":0`, `"expected_revision":null`, 1),
		strings.Replace(request, `"reason":"Explicit source routing reason"`, `"reason":null`, 1),
		strings.TrimSuffix(request, "}") + `,"expected_route_revision":null}`,
		strings.TrimSuffix(request, "}") + `,"target_project_id":"other"}`,
		strings.TrimSuffix(request, "}") + `,"finding":0}`,
	} {
		evidenceHTTPRequire(t, f.request("POST", path, f.writer, fmt.Sprintf("bad-route-%d", n), body), 400, "validation_failed")
	}
	evidenceHTTPRequire(t, f.request("POST", path, f.reader, "no-route-write", request), 403, "forbidden")
	if routeState(t, f, a) != before {
		t.Fatal("invalid or unauthorized HTTP changed route/evidence/receipts")
	}
	w := f.request("POST", path, f.writer, "route-http", request)
	body := evidenceHTTPRequire(t, w, 200, "")
	var created findingRouteReceipt
	if json.Unmarshal(w.Body.Bytes(), &created) != nil || len(body) != 4 || created.SourceRevision != 1 {
		t.Fatal(body)
	}
	evidenceHTTPRequire(t, f.request("POST", path, f.writer, "route-http", request), 200, "")
	routePath := base + "/routes/" + created.ID
	detail := evidenceHTTPRequire(t, f.request("GET", routePath, f.reader, "", ""), 200, "")
	finding := detail["source_finding"].(map[string]any)
	if finding["finding"] != float64(0) || finding["title"] != "Immutable finding 0" || finding["Title"] != nil || detail["source_anchor"] == nil {
		t.Fatal("route detail exposed parser casing or lost source index", detail)
	}
	evidenceHTTPRequire(t, f.request("POST", routePath+"/decisions", f.writer, "route-http-accept", `{"expected_revision":1,"disposition":"accepted","reason":"Receiving owner verified"}`), 200, "")
	evidenceHTTPRequire(t, f.request("POST", routePath+"/fixes", f.writer, "route-http-fix", `{"expected_revision":2,"expected_sha":"","fix_sha":"`+strings.Repeat("e", 40)+`","proof_note":"Focused receiver proof"}`), 200, "")
	evidenceHTTPRequire(t, f.request("POST", base+"/evidence/"+evidence+"/decisions", f.writer, "source-proof-loss", `{"expected_revision":1,"finding":0,"disposition":"rejected","reason":"Attempted removal","expected_route_revision":3}`), 409, "review_conflict")
	evidenceHTTPRequire(t, f.request("GET", routePath+"?refresh=true", f.reader, "", ""), 400, "validation_failed")
	evidenceHTTPRequire(t, f.request("GET", "/api/v1/projects/"+otherAttemptProject+"/reviews/routes/"+created.ID, f.other, "", ""), 404, "not_found")
	list := evidenceHTTPRequire(t, f.request("GET", base+"/routes?pull_number=4&limit=1", f.reader, "", ""), 200, "")
	if len(list["items"].([]any)) != 1 || strings.Contains(encoded(list), `"detail"`) || strings.Contains(encoded(list), `"routing_reason"`) {
		t.Fatal("ordinary route list expanded source detail", list)
	}
	// Null route guards cannot select the ordinary source decision path.
	evidenceHTTPRequire(t, f.request(http.MethodPost, base+"/evidence/"+evidence+"/decisions", f.writer, "source-null-guard", `{"expected_revision":1,"finding":1,"disposition":"rejected","reason":"No issue","expected_route_revision":null}`), 400, "validation_failed")
}
