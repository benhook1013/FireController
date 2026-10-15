package reviewworkspace

import (
	"context"
	"path/filepath"
)

// Open reconstructs an already prepared workspace from its admitted journal
// identity. It never prepares source or changes the repository binding.
func (m *Manager) Open(ctx context.Context, attemptID, candidateSHA, baseSHA, mergeBaseSHA, patchSHA256, repositoryCanonical string) (Workspace, error) {
	if m == nil || ctx == nil || !validID(attemptID) || !validSHA(candidateSHA) || !validSHA(baseSHA) || !validSHA(mergeBaseSHA) || !lowerHex(patchSHA256, 64) || !validCanonicalRepository(repositoryCanonical) {
		return Workspace{}, errInput
	}
	w := Workspace{
		Directory:           filepath.Join(m.root, "workspaces", attemptID),
		ComparisonRef:       "refs/firecontroller/review-base/" + attemptID,
		CandidateSHA:        candidateSHA,
		BaseSHA:             baseSHA,
		MergeBaseSHA:        mergeBaseSHA,
		PatchSHA256:         patchSHA256,
		RepositoryCanonical: repositoryCanonical,
	}
	if m.Verify(ctx, w) != nil || ctx.Err() != nil {
		return Workspace{}, errVerification
	}
	return w, nil
}
