package reviewexec

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strconv"
	"time"

	"github.com/benhook1013/FireController/internal/reviewprovider"
	"github.com/benhook1013/FireController/internal/strictjson"
)

// HostedEvidenceMetadata binds a proven final capture to its provider inventory.
// It does not establish current coverage, policy sufficiency or readiness.
type HostedEvidenceMetadata struct {
	PublicationID     string                         `json:"publication_id"`
	ProviderAttemptID string                         `json:"provider_attempt_id"`
	TriggerCommentID  int64                          `json:"trigger_comment_id"`
	ReviewIDs         []int64                        `json:"review_ids"`
	ObservedBaseSHA   string                         `json:"observed_base_sha"`
	Sources           []HostedFindingSource          `json:"sources"`
	StandingSummary   *HostedStandingSummaryMetadata `json:"standing_summary,omitempty"`
	SubmittedReview   *HostedSubmittedReviewMetadata `json:"submitted_review,omitempty"`
}

// Submitted reviews retain the identities actually supplied by GitHub and
// CodeRabbit rather than inventing the optional batched-publication identities.
type HostedSubmittedReviewMetadata struct {
	ProviderRunID            string `json:"provider_run_id"`
	AcknowledgementCommentID int64  `json:"acknowledgement_comment_id"`
}

type HostedStandingSummaryMetadata struct {
	CommentID     int64  `json:"comment_id"`
	ProviderRunID string `json:"provider_run_id"`
	UpdatedAt     string `json:"updated_at"`
}

// Retained metadata is a closed, versioned-by-shape sum type. Applying the
// local decoder here also protects existing journal readers that use Unmarshal.
func (h *HostedEvidenceMetadata) UnmarshalJSON(data []byte) error {
	type wire HostedEvidenceMetadata
	var value wire
	if err := strictjson.Decode(data, &value); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	for _, key := range []string{"publication_id", "provider_attempt_id", "trigger_comment_id", "review_ids", "observed_base_sha", "sources"} {
		raw, found := fields[key]
		if !found || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return strictjson.ErrSyntax
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
	if standing := h.StandingSummary; standing != nil {
		updated, err := time.Parse(time.RFC3339Nano, standing.UpdatedAt)
		if h.SubmittedReview != nil || h.PublicationID != "" || h.ProviderAttemptID != "" || h.ReviewIDs == nil || len(h.ReviewIDs) != 0 || len(h.Sources) != 0 || review.Findings == nil || len(review.Findings) != 0 || review.ReviewedFiles == nil || standing.CommentID <= 0 || standing.CommentID == h.TriggerCommentID || !hostedEvidenceUUID.MatchString(standing.ProviderRunID) || err != nil || updated.IsZero() {
			return invalid()
		}
		return nil
	}
	if submitted := h.SubmittedReview; submitted != nil {
		if h.PublicationID != "" || h.ProviderAttemptID != "" || len(h.ReviewIDs) != 1 || len(h.Sources) == 0 || !hostedEvidenceUUID.MatchString(submitted.ProviderRunID) || submitted.AcknowledgementCommentID <= 0 || submitted.AcknowledgementCommentID == h.TriggerCommentID {
			return invalid()
		}
	} else {
		if !hostedEvidenceUUID.MatchString(h.PublicationID) || !hostedEvidenceUUID.MatchString(h.ProviderAttemptID) || len(h.ReviewIDs) < 1 || len(h.ReviewIDs) > 200 {
			return invalid()
		}
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
		case "outside_diff", "duplicate", "additional":
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
