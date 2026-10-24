package reviews

import (
	"strings"
	"testing"
)

func TestCINotRequiredStoredDistinctAndComparisonReset(t *testing.T) {
	f := newAPIFixture(t)
	p := nativePull(1)
	p.State = "open"
	p.CIState = "not_required"
	f.observe(t, reviewRequest(0, []PullInput{p}), "not-required-import")
	if got := metadataRead(t, f, 1, "").Pull; got.CIState != "not_required" {
		t.Fatal("not-required observation lost", got.CIState)
	}
	metadata := sourcePull(1, p.Title)
	if _, err := publishStatus(t, f, sourcePins(t, f, 1), metadata, "unknown", "unknown"); err != nil {
		t.Fatal(err)
	}
	if got := metadataRead(t, f, 1, "").Pull; got.CIState != "unknown" {
		t.Fatal("unknown became not-required", got.CIState)
	}
	if _, err := publishStatus(t, f, sourcePins(t, f, 1), metadata, "not_required", "mergeable"); err != nil {
		t.Fatal(err)
	}
	old := metadataRead(t, f, 1, "")
	if old.Pull.CIState != "not_required" {
		t.Fatal("not-required source status lost", old.Pull.CIState)
	}
	for _, change := range []func(*PullInput){
		func(p *PullInput) { p.HeadSHA = strings.Repeat("c", 40) },
		func(p *PullInput) { p.BaseSHA = strings.Repeat("d", 40) },
		func(p *PullInput) { p.BaseRef = "develop" },
	} {
		if _, err := publishStatus(t, f, sourcePins(t, f, 1), metadata, "not_required", "mergeable"); err != nil {
			t.Fatal(err)
		}
		change(&metadata)
		if _, err := publishStatus(t, f, sourcePins(t, f, 1), metadata, "", "unknown"); err != nil {
			t.Fatal(err)
		}
		if got := metadataRead(t, f, 1, "").Pull; got.CIState != "unknown" {
			t.Fatal("comparison movement retained old CI", got.CIState)
		}
	}
}

func TestCINotRequiredQueueAndTransferAdmission(t *testing.T) {
	for _, state := range []string{"not_required", "unknown", "optional", "NOT_REQUIRED"} {
		t.Run(state, func(t *testing.T) {
			want := state == "not_required" || state == "unknown"
			pull := nativePull(1)
			pull.CIState = state
			edit := queueEditRequest(0, []QueuePullInput{{Number: 1, Title: pull.Title, State: pull.State, CIState: state, Mergeability: pull.Mergeability}}, nil)
			if err := validateQueueEdit(&edit); (err == nil) != want {
				t.Fatal("queue edit CI enum", err)
			}
			transfer := transferRequest(0, TransferGroup{Source: "synthetic-ci", Pulls: []PullInput{pull}})
			if _, err := normalizeTransfer(&transfer); (err == nil) != want {
				t.Fatal("canonical transfer CI enum", err)
			}
		})
	}
}
