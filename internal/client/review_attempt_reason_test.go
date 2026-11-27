package client_test

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/client"
	domain "github.com/benhook1013/FireController/internal/reviews"
)

func TestReviewAttemptReasonActualStoredReadsRemainSinglePage(t *testing.T) {
	f := newReviewClientFixture(t)
	snapshot := f.importSnapshot(t, func(in *domain.ObservationRequest) {
		in.Queue[0].Attempts[0].State = "uncertain"
		in.Queue[0].Attempts[0].Reason = "hosted_finished_inventory_unproven"
	})
	before := f.calls.Load()
	queue, err := f.client.ReviewQueue(t.Context(), client.PageOptions{})
	if err != nil {
		t.Fatal("compact diagnostic read", err)
	}
	pull, err := f.client.ReviewPull(t.Context(), 1, snapshot)
	if err != nil {
		t.Fatal("pinned pull diagnostic read", err)
	}
	attempts, err := f.client.ReviewAttempts(t.Context(), 1, client.ReviewAttemptOptions{Snapshot: snapshot})
	if err != nil {
		t.Fatal("flat archive diagnostic read", err)
	}
	for _, value := range []any{queue.Items[0].LatestAttempts, pull.Pull.LatestAttempts, attempts.Items} {
		body, err := json.Marshal(value)
		if err != nil || !bytes.Contains(body, []byte(`"reason":"hosted_finished_inventory_unproven"`)) {
			t.Fatal("typed read lost safe diagnostic", err)
		}
	}
	if f.calls.Load()-before != 3 {
		t.Fatal("diagnostic caused extra reads", f.calls.Load()-before)
	}
}

func TestReviewAttemptReasonStrictCompactAndFlatWire(t *testing.T) {
	f := newReviewClientFixture(t)
	f.importSnapshot(t, func(in *domain.ObservationRequest) {
		in.Queue[0].Attempts[0].State = "uncertain"
		in.Queue[0].Attempts[0].Reason = "safe_code"
	})
	pull, err := f.client.ReviewPull(t.Context(), 1, "")
	if err != nil {
		t.Fatal(err)
	}
	archive, err := f.client.ReviewAttempts(t.Context(), 1, client.ReviewAttemptOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, path string
		value      any
		read       func(*client.Client) error
	}{
		{"compact", "/api/v1/projects/project-one/reviews/pulls/1", pull, func(c *client.Client) error { _, err := c.ReviewPull(t.Context(), 1, ""); return err }},
		{"archive", "/api/v1/projects/project-one/reviews/pulls/1/attempts", archive, func(c *client.Client) error {
			_, err := c.ReviewAttempts(t.Context(), 1, client.ReviewAttemptOptions{})
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := json.Marshal(tc.value)
			if err != nil {
				t.Fatal(err)
			}
			body := string(raw)
			for _, state := range []string{"held", "uncertain", "rate_limited", "failed"} {
				mock := newReviewMock(t, strings.Replace(body, `"state":"uncertain"`, `"state":"`+state+`"`, 1), tc.path)
				if err := tc.read(mock.client); err != nil {
					t.Fatal("safe diagnostic state rejected", state, err)
				}
			}
			for _, bad := range []string{`""`, ` null `, `"UPPER"`, `"unsafe/path"`, `"_invalid"`, `"a-b"`, `"a\u0085"`, `"` + strings.Repeat("a", 129) + `"`} {
				mock := newReviewMock(t, strings.Replace(body, `"reason":"safe_code"`, `"reason":`+bad, 1), tc.path)
				checkError(t, tc.read(mock.client), "invalid_response")
			}
			for _, state := range []string{"preparing", "posting", "active", "completed", "cancelled", "unknown"} {
				mock := newReviewMock(t, strings.Replace(body, `"state":"uncertain"`, `"state":"`+state+`"`, 1), tc.path)
				checkError(t, tc.read(mock.client), "invalid_response")
			}
			old := newReviewMock(t, strings.Replace(body, `,"reason":"safe_code"`, "", 1), tc.path)
			if err := tc.read(old.client); err != nil {
				t.Fatal("old absent diagnostic rejected", err)
			}
		})
	}
}

func TestReviewPullReadsVerifiedParentFromActualResponse(t *testing.T) {
	f := newReviewClientFixture(t)
	snapshot := f.importSnapshot(t, func(in *domain.ObservationRequest) {
		in.Queue[0].State = "open"
		in.Queue = append(in.Queue, domain.PullInput{
			Number: 2, Title: "Stacked child", State: "open",
			HeadSHA: strings.Repeat("d", 40), BaseRef: "feature/parent", BaseSHA: in.Queue[0].HeadSHA,
			CIState: "pending", Mergeability: "unknown", Complete: true,
			Runs: []domain.RunInput{}, Attempts: []domain.Attempt{},
		})
	})
	for i, head := range []string{"feature/parent", "feature/child"} {
		draft := false
		metadata, err := json.Marshal(domain.PullDisplayMetadata{
			IsDraft:  &draft,
			Branches: &domain.PullBranches{HeadRepositoryID: 701, HeadRef: head, DeliveryRef: "main"},
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.store.DB().Exec("UPDATE review_pull_versions SET metadata_json=? WHERE project_id=? AND number=?", string(metadata), f.project, i+1); err != nil {
			t.Fatal(err)
		}
	}
	parent, err := f.client.ReviewPull(t.Context(), 1, snapshot)
	if err != nil || parent.Pull.ParentPullNumber != 0 {
		t.Fatalf("read root: %#v, %v", parent.Pull, err)
	}
	child, err := f.client.ReviewPull(t.Context(), 2, snapshot)
	if err != nil || child.Pull.ParentPullNumber != 1 {
		t.Fatalf("read child: %#v, %v", child.Pull, err)
	}

	body, err := json.Marshal(child)
	if err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []string{
		`"parent_pull_number":2`,
		`"parent_pull_number":-1`,
		`"parent_pull_number":2147483648`,
		`"parent_pull_number":1,"unregistered_field":1`,
	} {
		mock := newReviewMock(t, strings.Replace(string(body), `"parent_pull_number":1`, invalid, 1), "/api/v1/projects/project-one/reviews/pulls/2")
		_, err := mock.client.ReviewPull(t.Context(), 2, "")
		checkError(t, err, "invalid_response")
	}
}
