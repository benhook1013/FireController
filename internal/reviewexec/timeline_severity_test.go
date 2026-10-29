package reviewexec

import (
	"bytes"
	"testing"

	"github.com/benhook1013/FireController/internal/reviews"
)

func TestTimelineSeverityPreservesFinalizedPartitionAfterRoutingAndCorrection(t *testing.T) {
	f, _, evidence, _ := routeProjectionFixture(t, true)
	read := func(snapshot string) (reviews.RunSummary, []byte) {
		t.Helper()
		var page reviews.RunsResponse
		body := projectionRead(t, f, "/pulls/2/runs?view=timeline&snapshot="+snapshot, &page)
		for _, run := range page.Items {
			if run.Source == "native-executor" && run.SourceRunID == evidence.ID {
				return run, body
			}
		}
		t.Fatal("native round missing")
		return reviews.RunSummary{}, nil
	}
	initial := routeProjectionRead(t, f, evidence.ID)
	_, initialBytes := read(initial.SnapshotID)
	adjudicationDecide(t, f, evidence.ID, "timeline-accept-first", 0, 0, "accepted")
	adjudicationDecide(t, f, evidence.ID, "timeline-accept-second", 1, 1, "accepted")
	body, err := f.e.FinalizeReviewEvidence(t.Context(), f.scope(t, attemptProject, f.writer, false), "timeline-finalize", evidence.ID, FinalizeReviewRequest{ExpectedRevision: adjudicationRevision(2)})
	adjudicationReceipt(t, body, err, evidence.ID, "finalized", 3)
	finalized, finalizedBytes := read("")
	if finalized.AcceptedSeverity == nil || !finalized.AcceptedSeverity.Complete || finalized.Accepted != 2 || finalized.AcceptedSeverity.Major+finalized.AcceptedSeverity.Minor+finalized.AcceptedSeverity.Trivial+finalized.AcceptedSeverity.Unknown+finalized.AcceptedSeverity.Critical != 2 {
		t.Fatal("original finalized partition missing", finalized)
	}
	finalizedSnapshot := routeProjectionRead(t, f, evidence.ID).SnapshotID
	createProjectionRoute(t, f, evidence.ID, "timeline-route-after-finalization", 3, 0)
	afterRoute, _ := read("")
	if afterRoute.AcceptedSeverity == nil || *afterRoute.AcceptedSeverity != *finalized.AcceptedSeverity || afterRoute.Accepted != 2 || afterRoute.Routed != 0 {
		t.Fatal("routing lost original accepted partition", afterRoute)
	}
	adjudicationDecide(t, f, evidence.ID, "timeline-correct-after-finalization", 4, 1, "rejected")
	current, _ := read("")
	if current.AcceptedSeverity == nil || *current.AcceptedSeverity != *finalized.AcceptedSeverity || current.Accepted != 2 {
		t.Fatal("later correction replaced original accepted partition", current)
	}
	detail := routeProjectionRead(t, f, evidence.ID)
	if detail.Findings[0].Disposition != "routed" || detail.Findings[1].Disposition != "rejected" {
		t.Fatal("timeline repair changed current decisions", detail.Findings)
	}
	if _, pinned := read(initial.SnapshotID); !bytes.Equal(pinned, initialBytes) {
		t.Fatal("future finalization changed original unfinalized snapshot")
	}
	if _, pinned := read(finalizedSnapshot); !bytes.Equal(pinned, finalizedBytes) {
		t.Fatal("later decisions changed original finalized snapshot")
	}
}
