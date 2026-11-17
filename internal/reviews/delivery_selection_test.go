package reviews

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/core"
)

func removeDeliverySelectionForLegacyFixture(t *testing.T, store *core.Store) {
	t.Helper()
	for _, statement := range []string{`DROP VIEW review_pulls`, `ALTER TABLE review_pull_versions DROP COLUMN delivery_selected`, HistoryReadViewSQLV10()["review_pulls"]} {
		if _, err := store.DB().Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSubagentFirstUseRetainedOutsideDeliveryAndLaterEnrollment(t *testing.T) {
	for _, fresh := range []bool{false, true} {
		t.Run(map[bool]string{false: "existing workspace", true: "new workspace"}[fresh], func(t *testing.T) {
			f, token, base := subagentFixture(t)
			if fresh {
				f = newAPIFixture(t)
				token = subagentWriter(t, f)
				base = "/api/v1/projects/" + f.project + "/reviews/subagent-reports"
			}
			in := subagentInput()
			in.PullNumber = 99
			first := subagentRecord(t, f, token, base, "first-use", in)
			if replay := subagentRecord(t, f, token, base, "first-use", in); replay != first {
				t.Fatal("duplicate first use")
			}
			state := subagentRead(t, f, f.readToken, base+"/"+first.ID)
			if state.PullNumber != 99 || state.Run.HeadSHA != in.HeadSHA || state.Run.StartedAt != in.StartedAt || state.Run.FinishedAt != in.FinishedAt || state.Findings[0].Detail != in.Findings[0].Detail {
				t.Fatal("lost original review", state)
			}
			checkQueue := func() {
				t.Helper()
				for _, suffix := range []string{"/queue", "/queue/active", "/queue?view=recent", "/queue?view=merged"} {
					w := f.call("GET", importBase(f)+suffix, f.readToken, "", nil)
					if w.Code != 200 {
						t.Fatal(suffix, w.Code, w.Body.String())
					}
					var page struct {
						Items []struct {
							Number int64 `json:"number"`
						} `json:"items"`
					}
					if json.Unmarshal(w.Body.Bytes(), &page) != nil {
						t.Fatal(w.Body.String())
					}
					for _, p := range page.Items {
						if p.Number == 99 {
							t.Fatal("side PR leaked into Delivery", suffix)
						}
					}
				}
			}
			checkQueue()
			// Parent current metadata is honestly unknown, rather than the reviewed head.
			var selected int
			var head, prState string
			if err := f.store.DB().QueryRow(`SELECT delivery_selected,head_sha,state FROM review_pull_versions WHERE project_id=? AND number=99`, f.project).Scan(&selected, &head, &prState); err != nil || selected != 0 || head != "" || prState != "unknown" {
				t.Fatal("invented source facts", err, selected, head, prState)
			}
			update := SubagentUpdateRequest{ExpectedRevision: &state.Revision, Decisions: []SubagentDecisionInput{{Finding: 0, Disposition: "accepted", Reason: "verified"}}, Finalize: true}
			w := f.call("POST", base+"/"+first.ID+"/updates", token, "finalize-side", update)
			if w.Code != 200 {
				t.Fatal(w.Code, w.Body.String())
			}
			checkQueue()
			current := subagentRead(t, f, f.readToken, base+"/"+first.ID)
			// A complete explicit queue replacement must retain the independent report.
			var revision int64
			f.store.DB().QueryRow(`SELECT revision FROM review_workspace WHERE project_id=?`, f.project).Scan(&revision)
			replacement := []PullInput{nativePull(1)}
			if !fresh {
				p := samplePull(7, "Independent review target")
				p.State = "open"
				p.HeadSHA = strings.Repeat("a", 40)
				p.Complete = false
				replacement = append(replacement, p)
			}
			f.observe(t, reviewRequest(revision, replacement), "replace-delivery")
			if got := subagentRead(t, f, f.readToken, base+"/"+first.ID); !reflect.DeepEqual(current, got) {
				t.Fatal("queue replacement changed independent review", got)
			}
			checkQueue()
			f.store.DB().QueryRow(`SELECT revision FROM review_workspace WHERE project_id=?`, f.project).Scan(&revision)
			q := QueuePullInput{Number: 99, Title: "Now selected for delivery", State: "open", HeadSHA: strings.Repeat("c", 40), BaseRef: "main", CIState: "unknown", Mergeability: "unknown"}
			if w := queueEditHTTP(f, queueEditRequest(revision, []QueuePullInput{q}, nil), "enroll-side"); w.Code != 201 {
				t.Fatal(w.Code, w.Body.String())
			}
			if got := subagentRead(t, f, f.readToken, base+"/"+first.ID); !reflect.DeepEqual(current, got) {
				t.Fatal("enrollment changed retained review")
			}
			var visible int
			if err := f.store.DB().QueryRow(`SELECT v.delivery_selected FROM review_current_pulls c JOIN review_pull_memberships m ON m.project_id=c.project_id AND m.number=c.number AND m.start_revision=c.start_revision JOIN review_pull_versions v ON v.project_id=m.project_id AND v.number=m.number AND v.version_revision=m.version_revision WHERE c.project_id=? AND c.number=99`, f.project).Scan(&visible); err != nil || visible != 1 {
				t.Fatal("not enrolled", visible, err)
			}
		})
	}
}
