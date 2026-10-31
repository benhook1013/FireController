package reviewexec

import (
	"github.com/benhook1013/FireController/internal/reviewprovider"
	"reflect"
	"testing"
)

func TestAcceptedSourceSeverityPartitionsAndExcludesOtherDecisions(t *testing.T) {
	evidence := ReviewEvidence{}
	for _, severity := range []string{"high", "low", "info", "critical", "medium", "unknown"} {
		evidence.Capture.Review.Findings = append(evidence.Capture.Review.Findings, reviewprovider.ParsedFinding{Severity: severity})
	}
	state := ReviewAdjudication{}
	for i := range evidence.Capture.Review.Findings {
		state.Findings = append(state.Findings, FindingDecision{Finding: int64(i), Disposition: "accepted"})
	}
	got := acceptedSourceSeverity(evidence, state)
	want := &reviewprovider.CheckpointAcceptedSeverity{Major: 1, Minor: 1, Trivial: 1, Critical: 1, Unknown: 2}
	if !reflect.DeepEqual(got, want) || !got.Valid(6) {
		t.Fatal(got, want)
	}
	state.Findings[0].Disposition = "routed"
	state.Findings[1].Disposition = "rejected"
	got = acceptedSourceSeverity(evidence, state)
	if got.Major != 0 || got.Minor != 0 || !got.Valid(4) {
		t.Fatal(got)
	}
}

func TestCheckpointAcceptedSeverityRemainsOriginalAfterCorrection(t *testing.T) {
	f, _, id := checkpointFixture(t, 2)
	original := adjudicationRead(t, f, id)
	if original.FrozenAcceptedSeverity == nil || !original.FrozenAcceptedSeverity.Valid(2) {
		t.Fatal(original)
	}
	frozen := *original.FrozenAcceptedSeverity
	adjudicationDecide(t, f, id, "severity-later-correction", original.Revision, 0, "rejected")
	current := adjudicationRead(t, f, id)
	if current.FrozenAcceptedSeverity == nil || *current.FrozenAcceptedSeverity != frozen || current.FrozenCounts.Accepted != 2 {
		t.Fatal("correction changed original severity", current)
	}
	publication, _ := checkpointRequest(t, f, id, "publish-after-severity-correction")
	var body string
	if err := f.store.DB().QueryRow(`SELECT body_json FROM review_checkpoint_publications WHERE id=?`, publication.ID).Scan(&body); err != nil {
		t.Fatal(err)
	}
	record, err := decodeCheckpoint(body)
	if err != nil || record.Payload.AcceptedSeverity == nil || *record.Payload.AcceptedSeverity != frozen {
		t.Fatal(record, err)
	}
}

func TestAdjudicationHTTPKeepsPrivateFrozenDisplayMetadataOffWire(t *testing.T) {
	f, a, _, path := evidenceHTTPFixture(t, 0)
	adjudicationComplete(t, f, a)
	evidenceHTTPRequire(t, f.request("POST", path+"/finalization", f.writer, "severity-finalize-empty", `{"expected_revision":0}`), 200, "")
	value := evidenceHTTPRequire(t, f.request("GET", path+"/adjudication", f.reader, "", ""), 200, "")
	if len(value) != 5 || value["frozen_accepted_severity"] != nil {
		t.Fatal("private display metadata leaked to agent contract", value)
	}
}
