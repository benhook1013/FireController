package reviewexec

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strconv"

	"github.com/benhook1013/FireController/internal/reviewprovider"
	"github.com/benhook1013/FireController/internal/strictjson"
)

// HostedEvidenceMetadata binds a proven final capture to its provider inventory.
// It does not establish current coverage, policy sufficiency or readiness.
type HostedEvidenceMetadata struct {
	StandingZero      *HostedStandingZeroMetadata `json:"standing_zero,omitempty"`
	PublicationID     string                      `json:"publication_id,omitempty"`
	ProviderAttemptID string                      `json:"provider_attempt_id,omitempty"`
	TriggerCommentID  int64                       `json:"trigger_comment_id"`
	ReviewIDs         []int64                     `json:"review_ids,omitempty"`
	ObservedBaseSHA   string                      `json:"observed_base_sha"`
	Sources           []HostedFindingSource       `json:"sources"`
}

// HostedStandingZeroMetadata preserves real issue-comment identities. It is
// mutually exclusive with marked review batches and carries no finding counts.
type HostedStandingZeroMetadata struct {
	BaselineCaptureID     string `json:"baseline_capture_id"`
	BaselineCaptureSHA256 string `json:"baseline_capture_sha256"`
	CommentID             int64  `json:"comment_id"`
	CompletionCommentID   int64  `json:"completion_comment_id"`
	RunID                 string `json:"run_id"`
}

func (h *HostedEvidenceMetadata) UnmarshalJSON(data []byte) error {
	type plain HostedEvidenceMetadata
	var value plain
	if err := strictjson.Decode(data, &value); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(data, &fields) != nil || fields == nil {
		return strictjson.ErrSyntax
	}
	if raw, present := fields["standing_zero"]; present {
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return strictjson.ErrSyntax
		}
		for _, key := range []string{"publication_id", "provider_attempt_id", "review_ids"} {
			if _, mixed := fields[key]; mixed {
				return strictjson.ErrSyntax
			}
		}
	} else {
		for _, key := range []string{"publication_id", "provider_attempt_id", "review_ids"} {
			if _, present := fields[key]; !present {
				return strictjson.ErrSyntax
			}
		}
	}
	*h = HostedEvidenceMetadata(value)
	return nil
}

type HostedFindingSource struct {
	SourceKey   string `json:"source_key"`
	Fingerprint string `json:"fingerprint"`
	Kind        string `json:"kind"`
	Side        string `json:"side"`
	ReviewID    int64  `json:"review_id"`
	CommentID   int64  `json:"comment_id"`
}

var hostedEvidenceUUID = regexp.MustCompile(`^[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}$`)
var hostedEvidenceFingerprint = regexp.MustCompile(`^[a-f0-9]{24}$`)

func validateHostedEvidenceMetadata(h *HostedEvidenceMetadata, anchor Anchor, review reviewprovider.ParsedReview) error {
	if h == nil || h.TriggerCommentID <= 0 ||
		h.Sources == nil || len(h.Sources) != len(review.Findings) || len(h.Sources) > 100 || review.Outcome != "completed" || !review.Complete || len(review.ReviewedFiles) != 0 || review.ReportedFindings != int64(len(review.Findings)) ||
		!sha.MatchString(h.ObservedBaseSHA) || h.ObservedBaseSHA != anchor.ParentSHA && h.ObservedBaseSHA != anchor.MergeBaseSHA {
		return invalid()
	}
	if zero := h.StandingZero; zero != nil {
		if !hash.MatchString(zero.BaselineCaptureSHA256) || zero.BaselineCaptureID != "review_capture_"+zero.BaselineCaptureSHA256[:32] || h.PublicationID != "" || h.ProviderAttemptID != "" || len(h.ReviewIDs) != 0 || zero.CommentID <= 0 || zero.CompletionCommentID <= 0 || zero.CommentID == zero.CompletionCommentID || zero.CommentID == h.TriggerCommentID || zero.CompletionCommentID == h.TriggerCommentID || !hostedEvidenceUUID.MatchString(zero.RunID) || review.Findings == nil || len(review.Findings) != 0 || review.ReviewedFiles == nil || len(h.Sources) != 0 || review.ReportedFindings != 0 {
			return invalid()
		}
		return nil
	}
	if !hostedEvidenceUUID.MatchString(h.PublicationID) || !hostedEvidenceUUID.MatchString(h.ProviderAttemptID) || len(h.ReviewIDs) < 1 || len(h.ReviewIDs) > 200 {
		return invalid()
	}
	reviews := make(map[int64]bool, len(h.ReviewIDs))
	for _, id := range h.ReviewIDs {
		if id <= 0 || reviews[id] {
			return invalid()
		}
		reviews[id] = true
	}
	fingerprints := make(map[string]bool, len(h.Sources))
	for index, source := range h.Sources {
		if !reviews[source.ReviewID] || !hostedEvidenceFingerprint.MatchString(source.Fingerprint) || fingerprints[source.Fingerprint] || source.Side != "" && source.Side != "LEFT" && source.Side != "RIGHT" {
			return invalid()
		}
		fingerprints[source.Fingerprint] = true
		finding := review.Findings[index]
		if finding.Path == "" {
			return invalid()
		}
		var key string
		switch source.Kind {
		case "inline":
			if source.CommentID <= 0 {
				return invalid()
			}
			key = "hosted-comment:" + strconv.FormatInt(source.CommentID, 10) + ":" + source.Fingerprint
		case "outside_diff", "duplicate":
			if source.CommentID != 0 || finding.StartLine <= 0 || finding.EndLine < finding.StartLine {
				return invalid()
			}
			key = "hosted-review:" + strconv.FormatInt(source.ReviewID, 10) + ":" + source.Kind + ":" + source.Fingerprint
		default:
			return invalid()
		}
		if source.SourceKey != key {
			return invalid()
		}
	}
	return nil
}

// validateHostedEvidenceBinding checks retained posting facts in the caller's
// transaction; common evidence validation owns anchor and capability matching.
func validateHostedEvidenceBinding(a Attempt, o Operation, in ReviewEvidenceInput) error {
	if a.State != "active" && a.State != "uncertain" {
		return invalid()
	}
	return validateHostedEvidenceIdentity(a, o, in)
}

func validateHostedEvidenceIdentity(a Attempt, o Operation, in ReviewEvidenceInput) error {
	if o.Channel != "hosted" || in.Hosted == nil {
		return invalid()
	}
	intent, err := reviewprovider.HostedTriggerBody(reviewprovider.HostedTriggerIntent{OperationID: o.ID, AttemptID: a.ID, HeadSHA: o.HeadSHA})
	if err != nil || a.Intent != intent || a.ExternalID != strconv.FormatInt(in.Hosted.TriggerCommentID, 10) {
		return invalid()
	}
	return nil
}
