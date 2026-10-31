package reviewexec

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func operationListFixture(t *testing.T) *attemptFixture {
	t.Helper()
	f := newAttemptFixture(t)
	f.configure(t, attemptProject, "test-capability", 0, false)
	f.configure(t, otherAttemptProject, "test-capability", 0, false)
	return f
}

func seedOperationListRow(t *testing.T, f *attemptFixture, n int, project, state, channel string, pull int64) string {
	t.Helper()
	id := fmt.Sprintf("review_operation_%032x", n)
	at := "2026-10-23T12:00:00Z"
	_, err := f.store.DB().Exec(`INSERT INTO review_operations(id,project_id,actor_id,worker_id,configuration_revision,pull_number,channel,head_sha,state,revision,reason,attempt_id,created_at,updated_at) VALUES(?,?,'private-actor','worker-list',1,?,?,?, ?,1,'','',?,?)`, id, project, pull, channel, strings.Repeat("a", 40), state, at, at)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func listOperationPage(t *testing.T, f *attemptFixture, query string) OperationsPage {
	t.Helper()
	w := f.request(http.MethodGet, "/api/v1/projects/"+attemptProject+"/reviews/operations"+query, f.reader, "", "")
	var page OperationsPage
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &page) != nil {
		t.Fatal(w.Code, w.Body.String())
	}
	for _, private := range []string{"actor_id", "private-actor", "configuration_revision", "policy_revision", "allowance", "capability_fingerprint"} {
		if strings.Contains(w.Body.String(), private) {
			t.Fatal("list leaked selected-detail fields", private)
		}
	}
	return page
}

func TestOperationListDefaultAllFiltersKeysetAndMutableState(t *testing.T) {
	f := operationListFixture(t)
	if page := listOperationPage(t, f, ""); page.Items == nil || len(page.Items) != 0 || page.HasMore || page.Cursor != "" {
		t.Fatal(page)
	}
	states := []string{"queued", "preparing", "posting", "active", "uncertain", "held", "held", "completed", "failed", "cancelled", "rate_limited"}
	ids := make([]string, len(states))
	for i, state := range states {
		channel := "cli"
		if i%2 != 0 {
			channel = "hosted"
		}
		ids[i] = seedOperationListRow(t, f, i+1, attemptProject, state, channel, int64(2+i%2))
	}
	seedOperationListRow(t, f, 99, otherAttemptProject, "queued", "cli", 2)
	// Only the second held operation retains a matching execution reservation.
	aid := "review_attempt_" + strings.Repeat("b", 32)
	admissionSQL(t, f, `UPDATE review_operations SET attempt_id=? WHERE id=?`, aid, ids[6])
	admissionSQL(t, f, `INSERT INTO review_execution_attempts(id,operation_id,project_id,provider,account,state,revision,anchor_json,intent,external_id) VALUES(?,?,?,'synthetic','list-fixture','held',1,'{}','','')`, aid, ids[6], attemptProject)
	admissionSQL(t, f, `INSERT INTO review_execution_reservations(provider,repository_id,pull_number,action_class,account,attempt_id) VALUES('synthetic',801,2,'cli','list-fixture',?)`, aid)
	page := listOperationPage(t, f, "")
	if len(page.Items) != 5 || page.Items[0].ID != ids[6] || page.Items[4].ID != ids[1] || !page.HasMore || page.Cursor == "" {
		t.Fatal(page)
	}
	next := listOperationPage(t, f, "?cursor="+url.QueryEscape(page.Cursor))
	if len(next.Items) != 1 || next.Items[0].ID != ids[0] || next.HasMore || next.Cursor != "" {
		t.Fatal(next)
	}
	all := listOperationPage(t, f, "?status=all&limit=100")
	if len(all.Items) != len(states) || all.Items[0].ID != ids[10] || all.HasMore {
		t.Fatal(all)
	}
	filtered := listOperationPage(t, f, "?status=all&channel=hosted&pull_number=3")
	if len(filtered.Items) != 5 || filtered.HasMore {
		t.Fatal(filtered)
	}
	for _, o := range filtered.Items {
		if o.Channel != "hosted" || o.PullNumber != 3 {
			t.Fatal(o)
		}
	}
	// A completed older row disappears from pending continuation. Newer insertions
	// stay before the saved key; a cursor does not freeze mutable operation state.
	admissionSQL(t, f, `UPDATE review_operations SET state='completed' WHERE id=?`, ids[0])
	seedOperationListRow(t, f, 100, attemptProject, "queued", "cli", 2)
	if next := listOperationPage(t, f, "?cursor="+url.QueryEscape(page.Cursor)); len(next.Items) != 0 || next.HasMore {
		t.Fatal(next)
	}
	for n := 101; n <= 201; n++ {
		seedOperationListRow(t, f, n, attemptProject, "queued", "cli", 2)
	}
	maximum := listOperationPage(t, f, "?status=all&limit=100")
	if len(maximum.Items) != 100 || !maximum.HasMore || maximum.Cursor == "" {
		t.Fatal("maximum page boundary", maximum)
	}
	remainder := listOperationPage(t, f, "?status=all&limit=100&cursor="+url.QueryEscape(maximum.Cursor))
	if len(remainder.Items) != 13 || remainder.HasMore || remainder.Cursor != "" {
		t.Fatal("maximum page continuation", remainder)
	}
}

func TestOperationListHTTPAdmissionScopeRevocationAndCorruption(t *testing.T) {
	f := operationListFixture(t)
	for i := 1; i <= 6; i++ {
		seedOperationListRow(t, f, i, attemptProject, "queued", "cli", 2)
	}
	base := "/api/v1/projects/" + attemptProject + "/reviews/operations"
	first := listOperationPage(t, f, "")
	for _, query := range []string{"?limit=0", "?limit=101", "?limit=", "?pull_number=0", "?pull_number=01", "?pull_number=2147483648", "?channel=other", "?channel=", "?status=done", "?status=", "?cursor=", "?cursor=bad", "?limit=1&limit=2", "?unknown=1", "?Limit=1", "?limit=%zz"} {
		if w := f.request("GET", base+query, f.reader, "", ""); w.Code != 400 {
			t.Fatal(query, w.Code, w.Body.String())
		}
	}
	for _, query := range []string{"&status=all", "&channel=cli", "&pull_number=2"} {
		if w := f.request("GET", base+"?cursor="+url.QueryEscape(first.Cursor)+query, f.reader, "", ""); w.Code != 400 {
			t.Fatal("filter cursor accepted", w.Code)
		}
	}
	if w := f.request("GET", base+"?cursor="+url.QueryEscape(first.Cursor), f.writer, "", ""); w.Code != 400 {
		t.Fatal("foreign principal cursor accepted", w.Code)
	}
	if w := f.request("GET", strings.Replace(base, attemptProject, otherAttemptProject, 1), f.reader, "", ""); w.Code != 403 {
		t.Fatal("foreign project visible", w.Code)
	}
	if w := f.request("GET", strings.Replace(base, attemptProject, otherAttemptProject, 1)+"?cursor="+url.QueryEscape(first.Cursor), f.other, "", ""); w.Code != 400 {
		t.Fatal("foreign project cursor accepted", w.Code)
	}
	if w := f.request("GET", base+"?bad=1", "", "", ""); w.Code != 401 {
		t.Fatal("unauthenticated query precedes authority", w.Code)
	}
	scope := operationReadScope(t, f)
	admissionSQL(t, f, `UPDATE grants SET can_read=0 WHERE credential_id='exec-reader-cred'`)
	if _, err := f.e.ListOperations(t.Context(), scope, OperationListOptions{Cursor: first.Cursor}); err == nil {
		t.Fatal("stale authorized scope bypassed revoked grant")
	}
	if w := f.request("GET", base+"?cursor="+url.QueryEscape(first.Cursor), f.reader, "", ""); w.Code != 403 {
		t.Fatal("revoked continuation exposed state", w.Code)
	}
	admissionSQL(t, f, `UPDATE grants SET can_read=1 WHERE credential_id='exec-reader-cred'`)
	for _, field := range []string{"state", "head_sha", "reason", "created_at"} {
		t.Run(field, func(t *testing.T) {
			var original string
			id := fmt.Sprintf("review_operation_%032x", 6)
			if err := f.store.DB().QueryRow(`SELECT `+field+` FROM review_operations WHERE id=?`, id).Scan(&original); err != nil {
				t.Fatal(err)
			}
			admissionSQL(t, f, `UPDATE review_operations SET `+field+`='PRIVATE CORRUPTION' WHERE id=?`, id)
			w := f.request("GET", base+"?status=all", f.reader, "", "")
			if w.Code != 500 || strings.Contains(w.Body.String(), "PRIVATE") || strings.Contains(w.Body.String(), id) {
				t.Fatal("corrupt list exposed partial facts", w.Code, w.Body.String())
			}
			admissionSQL(t, f, `UPDATE review_operations SET `+field+`=? WHERE id=?`, original, id)
		})
	}
	// A valid signature cannot make an invalid keyset position trustworthy.
	bad := f.store.Cursor(scope, "reviews.operations:v1:0::pending", "bad|id")
	if _, err := f.e.ListOperations(t.Context(), scope, OperationListOptions{Cursor: bad}); err == nil {
		t.Fatal("malformed signed position accepted")
	}
}
