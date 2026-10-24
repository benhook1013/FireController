package reviewruntime

import (
	"context"
	"errors"
	"unicode/utf8"

	"github.com/benhook1013/FireController/internal/reviewexec"
	"github.com/benhook1013/FireController/internal/reviewworkspace"
)

// ApplicabilityVerifier belongs to trusted operator composition. It acquires
// only the four pinned objects in the existing private repository cache; it
// never starts a review or changes its evidence, findings or round accounting.
type ApplicabilityVerifier struct {
	manager    *reviewworkspace.Manager
	credential string
}

func NewApplicabilityVerifier(manager *reviewworkspace.Manager, credential string) (*ApplicabilityVerifier, error) {
	if manager == nil || !utf8.ValidString(credential) || len(credential) > maxCredentialTokenBytes || containsWhitespaceOrControl(credential) {
		return nil, errors.New("invalid applicability verifier configuration")
	}
	return &ApplicabilityVerifier{manager: manager, credential: credential}, nil
}

func (v *ApplicabilityVerifier) VerifyReviewApplicability(ctx context.Context, original reviewexec.Anchor, current reviewexec.SourcePlan) (reviewexec.EquivalentComparison, error) {
	if v == nil || v.manager == nil || original.RepositoryID != current.Repository.ID || original.PullNumber != current.Pull.Number || original.RepositoryID <= 0 || current.Pull.RepositoryID != current.Repository.ID || original.ParentSHA != original.BaseSHA || current.ParentSHA != current.Pull.BaseSHA {
		return reviewexec.EquivalentComparison{}, errors.New("review comparison unavailable")
	}
	for _, pair := range []reviewworkspace.ComparisonSnapshot{{BaseSHA: original.BaseSHA, CandidateSHA: original.CandidateSHA}, {BaseSHA: current.Pull.BaseSHA, CandidateSHA: current.Pull.HeadSHA}} {
		if err := v.manager.Fetch(ctx, current.Repository.ID, current.Repository.FullName, pair.CandidateSHA, pair.BaseSHA, v.credential); err != nil {
			return reviewexec.EquivalentComparison{}, err
		}
	}
	proof, err := v.manager.VerifyComparisonEquivalence(ctx, current.Repository.ID, reviewworkspace.ComparisonSnapshot{BaseSHA: original.BaseSHA, CandidateSHA: original.CandidateSHA}, reviewworkspace.ComparisonSnapshot{BaseSHA: current.Pull.BaseSHA, CandidateSHA: current.Pull.HeadSHA}, original.PatchSHA256)
	if err != nil {
		return reviewexec.EquivalentComparison{}, err
	}
	return reviewexec.EquivalentComparison(proof), nil
}
