package reviewexec

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/reviewprovider"
)

func TestHostedCheckpointFileEditRequiresOriginalPublishedJournal(t *testing.T) {
	f, oldAttempt, evidence, _ := checkpointHostedComparisonFixture(t)
	var publication string
	if err := f.store.DB().QueryRow(`SELECT id FROM review_checkpoint_publications WHERE evidence_id=?`, evidence).Scan(&publication); err != nil {
		t.Fatal(err)
	}
	// Model the previous renderer's genuinely accepted absent file fact, then
	// publish through the ordinary intent/confirmation path, not a pasted marker.
	old := checkpointRecordFor(t, f, publication)
	old.Payload.Files = nil
	rendered, err := reviewprovider.RenderCheckpoint(old.Payload)
	if err != nil {
		t.Fatal(err)
	}
	old.SemanticSHA256 = rendered.SemanticSHA256
	admissionSQL(t, f, `UPDATE review_checkpoint_publications SET body_json=? WHERE id=?`, encoded(old), publication)
	admissionSQL(t, f, `UPDATE review_checkpoint_revisions SET body_json=? WHERE publication_id=?`, encoded(old), publication)
	provider := &checkpointFake{}
	cancel, done := checkpointStart(t, f.e, provider)
	published := checkpointTerminal(t, f, publication)
	checkpointStop(t, cancel, done)
	if published.State != "published" {
		t.Fatal("prior checkpoint not attributed", published.State)
	}
	prior := provider.comments[0]
	current := f.start(t, attemptProject, f.enqueue(t, attemptProject, "later-hosted-request", "hosted", 2, 1))
	anchor := *oldAttempt.Anchor
	query, _, err := f.e.BeginHostedAllowanceQuery(t.Context(), attemptProject, current.ID, current.Revision, anchor, prior.Author, reviewprovider.Author{ID: 30, Login: "synthetic-review[bot]", Kind: "Bot"})
	if err != nil {
		t.Fatal(err)
	}
	query, err = f.e.ConfirmHostedAllowanceQuery(t.Context(), attemptProject, current.ID, current.Revision, hostedQueryComment(t, query))
	if err != nil {
		t.Fatal(err)
	}
	baseline := reviewprovider.HostedPublicationSnapshot{Complete: true, ObservedAt: time.Now().UTC(), IssueComments: []reviewprovider.IssueComment{prior}, Reviews: []reviewprovider.HostedReview{}, ReviewComments: []reviewprovider.ReviewComment{}}
	body, err := reviewprovider.HostedTriggerBody(reviewprovider.HostedTriggerIntent{OperationID: current.OperationID, AttemptID: current.ID, HeadSHA: anchor.CandidateSHA})
	if err != nil {
		t.Fatal(err)
	}
	current, err = f.e.Admit(t.Context(), attemptProject, current.ID, current.Revision, anchor, body)
	if err != nil {
		t.Fatal(err)
	}
	admitted, err := f.e.ReadHostedPostingBoundary(t.Context(), attemptProject, current.ID)
	if err != nil {
		t.Fatal(err)
	}
	current, err = f.e.Advance(t.Context(), attemptProject, current.ID, current.Revision, "active", "posted", "1000")
	if err != nil {
		t.Fatal(err)
	}
	triggerAt := admitted.Add(2 * time.Second).Truncate(time.Second)
	edited := prior
	edited.UpdatedAt = triggerAt.Add(time.Second)
	lines := strings.SplitN(prior.Body, "\n", 3)
	edited.Body = lines[0] + "\n99 files · " + lines[1] + "\n" + lines[2]
	snapshot := reviewprovider.HostedPublicationSnapshot{Complete: true, ObservedAt: triggerAt.Add(2 * time.Second), IssueComments: []reviewprovider.IssueComment{edited, {ID: 1000, Author: prior.Author, Body: body, CreatedAt: triggerAt, UpdatedAt: triggerAt}}, Reviews: []reviewprovider.HostedReview{}, ReviewComments: []reviewprovider.ReviewComment{}}
	for _, mode := range []string{"file annotation", "forged marker", "other actor", "changed summary", "unknown comment", "wrong count", "hidden command", "no baseline"} {
		t.Run(mode, func(t *testing.T) {
			input := snapshot
			input.IssueComments = append([]reviewprovider.IssueComment(nil), snapshot.IssueComments...)
			base := baseline
			base.IssueComments = append([]reviewprovider.IssueComment(nil), baseline.IssueComments...)
			switch mode {
			case "forged marker":
				input.IssueComments[0].Body = strings.Replace(input.IssueComments[0].Body, publication, "review_publication_"+strings.Repeat("f", 32), 1)
			case "other actor":
				input.IssueComments[0].Author.ID++
			case "changed summary":
				input.IssueComments[0].Body = strings.Replace(input.IssueComments[0].Body, "0 Found", "1 Found", 1)
			case "unknown comment":
				input.IssueComments[0].ID++
				base.IssueComments[0].ID++
			case "wrong count":
				input.IssueComments[0].Body = strings.Replace(input.IssueComments[0].Body, "99 files", "100 files", 1)
			case "hidden command":
				input.IssueComments[0].Body += "\n<!-- @coderabbitai review -->"
			case "no baseline":
				base.IssueComments = []reviewprovider.IssueComment{}
			}
			before := encoded(input)
			beforeBase := encoded(base)
			durable := checkpointSQLState(t, f)
			got, err := f.e.ProjectHostedCheckpointFileEdits(t.Context(), attemptProject, current.ID, base, input)
			if err != nil {
				t.Fatal(err)
			}
			expected := input
			expected.IssueComments = append([]reviewprovider.IssueComment(nil), input.IssueComments...)
			if mode == "file annotation" {
				expected.IssueComments[0].UpdatedAt = prior.UpdatedAt
			}
			if !reflect.DeepEqual(got, expected) || encoded(input) != before || encoded(base) != beforeBase || checkpointSQLState(t, f) != durable {
				t.Fatal("unproven edit qualified, source mutated or journal changed", mode)
			}
		})
	}
}
