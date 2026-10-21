package reviewexec

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/reviewprovider"
)

func hostedEvidenceMetadataFixture() (*HostedEvidenceMetadata, Anchor, reviewprovider.ParsedReview) {
	anchor := Anchor{ParentSHA: strings.Repeat("a", 40), MergeBaseSHA: strings.Repeat("b", 40), BaseSHA: strings.Repeat("c", 40)}
	sources := []HostedFindingSource{
		{Fingerprint: strings.Repeat("1", 24), Kind: "inline", ReviewID: 201, CommentID: 301, Side: "RIGHT"},
		{Fingerprint: strings.Repeat("2", 24), Kind: "outside_diff", ReviewID: 201},
		{Fingerprint: strings.Repeat("3", 24), Kind: "duplicate", ReviewID: 202, Side: "LEFT"},
	}
	for i := range sources {
		s := &sources[i]
		if s.Kind == "inline" {
			s.SourceKey = fmt.Sprintf("hosted-comment:%d:%s", s.CommentID, s.Fingerprint)
		} else {
			s.SourceKey = fmt.Sprintf("hosted-review:%d:%s:%s", s.ReviewID, s.Kind, s.Fingerprint)
		}
	}
	h := &HostedEvidenceMetadata{PublicationID: "11111111-1111-1111-1111-111111111111", ProviderAttemptID: "22222222-2222-2222-2222-222222222222", TriggerCommentID: 101, ReviewIDs: []int64{201, 202}, ObservedBaseSHA: anchor.ParentSHA, Sources: sources}
	review := reviewprovider.ParsedReview{Outcome: "completed", Complete: true, ReportedFindings: 3, Findings: []reviewprovider.ParsedFinding{
		{Path: "source.go"}, // Unknown inline coordinates remain unknown.
		{Path: "outside.go", StartLine: 8, EndLine: 12},
		{Path: "duplicate.go", StartLine: 5, EndLine: 5},
	}}
	return h, anchor, review
}

func TestHostedEvidenceMetadataAcceptsExactSourcesAndScope(t *testing.T) {
	h, anchor, review := hostedEvidenceMetadataFixture()
	for _, base := range []string{anchor.ParentSHA, anchor.MergeBaseSHA} {
		h.ObservedBaseSHA = base
		if err := validateHostedEvidenceMetadata(h, anchor, review); err != nil {
			t.Fatal("proven parent/merge-base scope rejected", err)
		}
	}
	// Two atomic inline findings may share one comment and one provider review.
	// Neither root count nor review count substitutes for atomic finding count.
	h.Sources[1].Kind, h.Sources[1].CommentID = "inline", h.Sources[0].CommentID
	h.Sources[1].SourceKey = fmt.Sprintf("hosted-comment:%d:%s", h.Sources[1].CommentID, h.Sources[1].Fingerprint)
	review.Findings[1].StartLine, review.Findings[1].EndLine = 0, 0
	if err := validateHostedEvidenceMetadata(h, anchor, review); err != nil {
		t.Fatal("bundled independently fingerprinted inline findings rejected", err)
	}
	h.Sources = []HostedFindingSource{}
	review.Findings, review.ReportedFindings = nil, 0
	if err := validateHostedEvidenceMetadata(h, anchor, review); err != nil {
		t.Fatal("proven zero-finding capture rejected", err)
	}
}

func TestHostedEvidenceMetadataRejectsUnprovenOrConflictingFacts(t *testing.T) {
	cases := map[string]func(*HostedEvidenceMetadata, *Anchor, *reviewprovider.ParsedReview){
		"publication missing": func(h *HostedEvidenceMetadata, _ *Anchor, _ *reviewprovider.ParsedReview) { h.PublicationID = "" },
		"publication noncanonical": func(h *HostedEvidenceMetadata, _ *Anchor, _ *reviewprovider.ParsedReview) {
			h.PublicationID = "AAAAAAAA-1111-1111-1111-111111111111"
		},
		"provider attempt invalid": func(h *HostedEvidenceMetadata, _ *Anchor, _ *reviewprovider.ParsedReview) {
			h.ProviderAttemptID = "attempt"
		},
		"trigger missing":  func(h *HostedEvidenceMetadata, _ *Anchor, _ *reviewprovider.ParsedReview) { h.TriggerCommentID = 0 },
		"trigger negative": func(h *HostedEvidenceMetadata, _ *Anchor, _ *reviewprovider.ParsedReview) { h.TriggerCommentID = -1 },
		"reviews missing":  func(h *HostedEvidenceMetadata, _ *Anchor, _ *reviewprovider.ParsedReview) { h.ReviewIDs = nil },
		"reviews excessive": func(h *HostedEvidenceMetadata, _ *Anchor, _ *reviewprovider.ParsedReview) {
			h.ReviewIDs = make([]int64, 201)
		},
		"review nonpositive": func(h *HostedEvidenceMetadata, _ *Anchor, _ *reviewprovider.ParsedReview) { h.ReviewIDs[0] = 0 },
		"review repeated": func(h *HostedEvidenceMetadata, _ *Anchor, _ *reviewprovider.ParsedReview) {
			h.ReviewIDs[1] = h.ReviewIDs[0]
		},
		"sources null":   func(h *HostedEvidenceMetadata, _ *Anchor, _ *reviewprovider.ParsedReview) { h.Sources = nil },
		"source omitted": func(h *HostedEvidenceMetadata, _ *Anchor, _ *reviewprovider.ParsedReview) { h.Sources = h.Sources[:2] },
		"source unknown review": func(h *HostedEvidenceMetadata, _ *Anchor, _ *reviewprovider.ParsedReview) {
			h.Sources[0].ReviewID = 999
		},
		"fingerprint malformed": func(h *HostedEvidenceMetadata, _ *Anchor, _ *reviewprovider.ParsedReview) {
			h.Sources[0].Fingerprint = "bad"
		},
		"fingerprint noncanonical": func(h *HostedEvidenceMetadata, _ *Anchor, _ *reviewprovider.ParsedReview) {
			h.Sources[0].Fingerprint = strings.Repeat("A", 24)
		},
		"fingerprint reused across kinds and reviews": func(h *HostedEvidenceMetadata, _ *Anchor, _ *reviewprovider.ParsedReview) {
			h.Sources[2].Fingerprint = h.Sources[0].Fingerprint
			h.Sources[2].SourceKey = fmt.Sprintf("hosted-review:%d:duplicate:%s", h.Sources[2].ReviewID, h.Sources[2].Fingerprint)
		},
		"source key mismatch": func(h *HostedEvidenceMetadata, _ *Anchor, _ *reviewprovider.ParsedReview) {
			h.Sources[0].SourceKey += "extra"
		},
		"inline comment missing": func(h *HostedEvidenceMetadata, _ *Anchor, _ *reviewprovider.ParsedReview) { h.Sources[0].CommentID = 0 },
		"inline path missing":    func(_ *HostedEvidenceMetadata, _ *Anchor, r *reviewprovider.ParsedReview) { r.Findings[0].Path = "" },
		"summary comment present": func(h *HostedEvidenceMetadata, _ *Anchor, _ *reviewprovider.ParsedReview) {
			h.Sources[1].CommentID = 301
		},
		"summary path missing": func(_ *HostedEvidenceMetadata, _ *Anchor, r *reviewprovider.ParsedReview) { r.Findings[1].Path = "" },
		"summary range missing": func(_ *HostedEvidenceMetadata, _ *Anchor, r *reviewprovider.ParsedReview) {
			r.Findings[1].StartLine = 0
		},
		"summary end missing":    func(_ *HostedEvidenceMetadata, _ *Anchor, r *reviewprovider.ParsedReview) { r.Findings[1].EndLine = 0 },
		"summary range reversed": func(_ *HostedEvidenceMetadata, _ *Anchor, r *reviewprovider.ParsedReview) { r.Findings[1].EndLine = 7 },
		"kind unknown": func(h *HostedEvidenceMetadata, _ *Anchor, _ *reviewprovider.ParsedReview) {
			h.Sources[1].Kind = "summary"
		},
		"side unknown": func(h *HostedEvidenceMetadata, _ *Anchor, _ *reviewprovider.ParsedReview) {
			h.Sources[0].Side = "right"
		},
		"scope absent": func(h *HostedEvidenceMetadata, _ *Anchor, _ *reviewprovider.ParsedReview) { h.ObservedBaseSHA = "" },
		"scope foreign": func(h *HostedEvidenceMetadata, a *Anchor, _ *reviewprovider.ParsedReview) {
			h.ObservedBaseSHA = a.BaseSHA
		},
		"scope noncanonical": func(h *HostedEvidenceMetadata, _ *Anchor, _ *reviewprovider.ParsedReview) {
			h.ObservedBaseSHA = strings.Repeat("A", 40)
		},
		"partial outcome": func(_ *HostedEvidenceMetadata, _ *Anchor, r *reviewprovider.ParsedReview) { r.Outcome = "uncertain" },
		"incomplete":      func(_ *HostedEvidenceMetadata, _ *Anchor, r *reviewprovider.ParsedReview) { r.Complete = false },
		"fabricated file inventory": func(_ *HostedEvidenceMetadata, _ *Anchor, r *reviewprovider.ParsedReview) {
			r.ReviewedFiles = []string{"source.go"}
		},
		"atomic count mismatch": func(_ *HostedEvidenceMetadata, _ *Anchor, r *reviewprovider.ParsedReview) { r.ReportedFindings = 1 },
		"excessive atomic findings": func(h *HostedEvidenceMetadata, _ *Anchor, r *reviewprovider.ParsedReview) {
			h.Sources = make([]HostedFindingSource, 101)
			r.Findings, r.ReportedFindings = make([]reviewprovider.ParsedFinding, 101), 101
		},
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			h, anchor, review := hostedEvidenceMetadataFixture()
			change(h, &anchor, &review)
			if err := validateHostedEvidenceMetadata(h, anchor, review); err == nil {
				t.Fatal("unproven hosted metadata accepted")
			}
		})
	}
	h, anchor, review := hostedEvidenceMetadataFixture()
	if err := validateHostedEvidenceMetadata(nil, anchor, review); err == nil {
		t.Fatal("missing hosted metadata accepted")
	}
	h.Sources, review.Findings, review.ReportedFindings = nil, nil, 0
	if err := validateHostedEvidenceMetadata(h, anchor, review); err == nil {
		t.Fatal("null sources accepted for zero findings")
	}
}

func TestHostedEvidenceBindingRequiresExactPostedIntent(t *testing.T) {
	fixture := func() (Attempt, Operation, ReviewEvidenceInput) {
		h, anchor, review := hostedEvidenceMetadataFixture()
		o := Operation{ID: "review_operation_" + strings.Repeat("a", 32), Channel: "hosted", HeadSHA: strings.Repeat("b", 40)}
		a := Attempt{ID: "review_attempt_" + strings.Repeat("c", 32), State: "active", ExternalID: "101"}
		body, err := reviewprovider.HostedTriggerBody(reviewprovider.HostedTriggerIntent{OperationID: o.ID, AttemptID: a.ID, HeadSHA: o.HeadSHA})
		if err != nil {
			t.Fatal(err)
		}
		a.Intent = body
		return a, o, ReviewEvidenceInput{Hosted: h, ExpectedAnchor: anchor, Review: review}
	}
	for _, state := range []string{"active", "uncertain"} {
		a, o, in := fixture()
		a.State = state
		if err := validateHostedEvidenceBinding(a, o, in); err != nil {
			t.Fatal("exact posted binding rejected", state, err)
		}
	}
	for name, change := range map[string]func(*Attempt, *Operation, *ReviewEvidenceInput){
		"wrong channel":         func(_ *Attempt, o *Operation, _ *ReviewEvidenceInput) { o.Channel = "cli" },
		"metadata missing":      func(_ *Attempt, _ *Operation, in *ReviewEvidenceInput) { in.Hosted = nil },
		"unposted preparing":    func(a *Attempt, _ *Operation, _ *ReviewEvidenceInput) { a.State = "preparing" },
		"posting not confirmed": func(a *Attempt, _ *Operation, _ *ReviewEvidenceInput) { a.State = "posting" },
		"already completed":     func(a *Attempt, _ *Operation, _ *ReviewEvidenceInput) { a.State = "completed" },
		"intent edited":         func(a *Attempt, _ *Operation, _ *ReviewEvidenceInput) { a.Intent += "\n" },
		"operation changed": func(_ *Attempt, o *Operation, _ *ReviewEvidenceInput) {
			o.ID = "review_operation_" + strings.Repeat("d", 32)
		},
		"attempt changed": func(a *Attempt, _ *Operation, _ *ReviewEvidenceInput) {
			a.ID = "review_attempt_" + strings.Repeat("d", 32)
		},
		"head changed":                  func(_ *Attempt, o *Operation, _ *ReviewEvidenceInput) { o.HeadSHA = strings.Repeat("d", 40) },
		"operation malformed":           func(_ *Attempt, o *Operation, _ *ReviewEvidenceInput) { o.ID = "invalid" },
		"trigger absent":                func(a *Attempt, _ *Operation, _ *ReviewEvidenceInput) { a.ExternalID = "" },
		"trigger foreign":               func(a *Attempt, _ *Operation, _ *ReviewEvidenceInput) { a.ExternalID = "999" },
		"trigger not canonical decimal": func(a *Attempt, _ *Operation, _ *ReviewEvidenceInput) { a.ExternalID = "0101" },
	} {
		t.Run(name, func(t *testing.T) {
			a, o, in := fixture()
			change(&a, &o, &in)
			if err := validateHostedEvidenceBinding(a, o, in); err == nil {
				t.Fatal("foreign or unposted binding accepted")
			}
		})
	}
}

func TestHostedEvidenceMetadataJSONFields(t *testing.T) {
	h, _, _ := hostedEvidenceMetadataFixture()
	data, err := json.Marshal(h)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"publication_id", "provider_attempt_id", "trigger_comment_id", "review_ids", "observed_base_sha", "sources"} {
		if _, ok := fields[key]; !ok {
			t.Fatal("missing metadata JSON field", key)
		}
	}
	var sources []map[string]json.RawMessage
	if err := json.Unmarshal(fields["sources"], &sources); err != nil || len(sources) != len(h.Sources) {
		t.Fatal("source metadata missing", err)
	}
	for _, key := range []string{"source_key", "fingerprint", "kind", "side", "review_id", "comment_id"} {
		if _, ok := sources[0][key]; !ok {
			t.Fatal("missing source JSON field", key)
		}
	}
	var loaded HostedEvidenceMetadata
	if err := json.Unmarshal(data, &loaded); err != nil || !reflect.DeepEqual(&loaded, h) {
		t.Fatal("hosted source identity changed in round trip", err)
	}
}

func TestHostedSubmittedReviewMetadataIsDistinctAndPositive(t *testing.T) {
	fixture := func() (*HostedEvidenceMetadata, Anchor, reviewprovider.ParsedReview) {
		h, a, r := hostedEvidenceMetadataFixture()
		h.PublicationID, h.ProviderAttemptID = "", ""
		h.ReviewIDs = []int64{201}
		h.Sources = h.Sources[:1]
		r.Findings, r.ReportedFindings = r.Findings[:1], 1
		h.SubmittedReview = &HostedSubmittedReviewMetadata{ProviderRunID: "33333333-3333-3333-3333-333333333333", AcknowledgementCommentID: 102}
		return h, a, r
	}
	h, a, r := fixture()
	if err := validateHostedEvidenceMetadata(h, a, r); err != nil {
		t.Fatal("real submitted-review identity rejected", err)
	}
	raw, err := json.Marshal(h)
	if err != nil {
		t.Fatal(err)
	}
	var decoded HostedEvidenceMetadata
	if err = json.Unmarshal(raw, &decoded); err != nil || !reflect.DeepEqual(h, &decoded) {
		t.Fatal("submitted identities lost on retained read", err)
	}
	cases := map[string]func(*HostedEvidenceMetadata, *reviewprovider.ParsedReview){
		"invented publication": func(h *HostedEvidenceMetadata, _ *reviewprovider.ParsedReview) {
			h.PublicationID = "11111111-1111-1111-1111-111111111111"
		},
		"invented attempt": func(h *HostedEvidenceMetadata, _ *reviewprovider.ParsedReview) {
			h.ProviderAttemptID = "22222222-2222-2222-2222-222222222222"
		},
		"multiple reviews": func(h *HostedEvidenceMetadata, _ *reviewprovider.ParsedReview) {
			h.ReviewIDs = append(h.ReviewIDs, 202)
		},
		"missing run": func(h *HostedEvidenceMetadata, _ *reviewprovider.ParsedReview) { h.SubmittedReview.ProviderRunID = "" },
		"missing ack": func(h *HostedEvidenceMetadata, _ *reviewprovider.ParsedReview) {
			h.SubmittedReview.AcknowledgementCommentID = 0
		},
		"ack is trigger": func(h *HostedEvidenceMetadata, _ *reviewprovider.ParsedReview) {
			h.SubmittedReview.AcknowledgementCommentID = h.TriggerCommentID
		},
		"standing mixed": func(h *HostedEvidenceMetadata, _ *reviewprovider.ParsedReview) {
			h.StandingSummary = &HostedStandingSummaryMetadata{}
		},
		"zero inventory": func(h *HostedEvidenceMetadata, r *reviewprovider.ParsedReview) {
			h.Sources = []HostedFindingSource{}
			r.Findings = []reviewprovider.ParsedFinding{}
			r.ReportedFindings = 0
		},
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			h, a, r := fixture()
			change(h, &r)
			if validateHostedEvidenceMetadata(h, a, r) == nil {
				t.Fatal("conflicting submitted identity accepted")
			}
		})
	}
}
