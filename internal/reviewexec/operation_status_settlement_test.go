package reviewexec

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestOperationStatusSettlementTargetCurrentAdmission(t *testing.T) {
	f, a, _ := evidenceFixture(t, "hosted", false)
	scope := evidenceReadScope(t, f, attemptProject, f.reader)
	read := func(want *Attempt) {
		t.Helper()
		before := evidenceState(t, f, a)
		w := statusHTTP(t, f, a.OperationID, "", "")
		if w.Code != 200 {
			t.Fatalf("status: %d", w.Code)
		}
		var fields map[string]json.RawMessage
		if json.Unmarshal(w.Body.Bytes(), &fields) != nil {
			t.Fatal("invalid status")
		}
		raw, present := fields["settlement_target"]
		if want == nil {
			if present {
				t.Fatal("unadmitted attempt supplied settlement guards")
			}
		} else {
			var target struct {
				Revision   int64  `json:"attempt_revision"`
				Repository int64  `json:"repository_id"`
				Head       string `json:"head_sha"`
				External   string `json:"external_id"`
			}
			if !present || json.Unmarshal(raw, &target) != nil || target.Revision != want.Revision || target.Repository != want.Anchor.RepositoryID || target.Head != want.Anchor.CandidateSHA || target.External != want.ExternalID {
				t.Fatal("current original attempt guards missing or changed")
			}
			if _, present := fields["evidence_id"]; present {
				t.Fatal("fixture unexpectedly has evidence")
			}
		}
		if before != evidenceState(t, f, a) {
			t.Fatal("status read mutated durable state")
		}
	}
	read(nil)
	var err error
	a, err = f.e.Admit(t.Context(), attemptProject, a.ID, a.Revision, attemptAnchor(attemptProject, 2), "private-intent")
	if err != nil {
		t.Fatal(err)
	}
	read(&a)
	a, err = f.e.Advance(t.Context(), attemptProject, a.ID, a.Revision, "active", "trigger_posted", "123456789")
	if err != nil {
		t.Fatal(err)
	}
	read(&a)
	a, err = f.e.Advance(t.Context(), attemptProject, a.ID, a.Revision, "uncertain", "observation_pending", "")
	if err != nil {
		t.Fatal(err)
	}
	read(&a)
	original := encoded(a.Anchor)
	for _, bad := range []string{strings.TrimSuffix(original, "}") + `,"repository_id":801}`, `{}`, `{"repository_id":801}`, encoded(func() *Anchor { v := *a.Anchor; v.CandidateSHA = a.Anchor.BaseSHA; return &v }()), encoded(func() *Anchor { v := *a.Anchor; v.PullNumber++; return &v }()), strings.Repeat("x", 2049)} {
		if _, err := f.store.DB().Exec(`UPDATE review_execution_attempts SET anchor_json=? WHERE id=?`, bad, a.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := f.e.ReadOperationStatus(t.Context(), scope, a.OperationID); err == nil {
			t.Fatal("invalid original anchor supplied guards")
		}
	}
	if _, err := f.store.DB().Exec(`UPDATE review_execution_attempts SET anchor_json=? WHERE id=?`, original, a.ID); err != nil {
		t.Fatal(err)
	}
	read(&a)
	// An attempt linked to another operation cannot supply this operation's
	// guards, even without any evidence.
	other := f.enqueue(t, attemptProject, "other-settlement-target", "hosted", 4, 1)
	if _, err := f.store.DB().Exec(`UPDATE review_execution_attempts SET operation_id=? WHERE id=?`, other.ID, a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.e.ReadOperationStatus(t.Context(), scope, a.OperationID); err == nil {
		t.Fatal("foreign attempt supplied guards")
	}
	if _, err := f.store.DB().Exec(`UPDATE review_execution_attempts SET operation_id=? WHERE id=?`, a.OperationID, a.ID); err != nil {
		t.Fatal(err)
	}
	// A previously authorized scope must be reauthorized before emitting guards.
	if _, err := f.store.DB().Exec(`UPDATE grants SET can_read=0 WHERE credential_id='exec-reader-cred'`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.e.ReadOperationStatus(t.Context(), scope, a.OperationID); err == nil {
		t.Fatal("revoked reader received settlement guards")
	}
}
