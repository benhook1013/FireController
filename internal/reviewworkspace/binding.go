package reviewworkspace

import (
	"context"
)

// BindRepository replaces the private cache origin in one owned clone with the
// freshly verified provider repository. CLI repository/account discovery must
// not mistake the cache's local filesystem path for the external identity.
// The operation performs no network request or source-tree change.
func (m *Manager) BindRepository(ctx context.Context, w Workspace, canonical string) (Workspace, error) {
	if m == nil || ctx == nil || !validCanonicalRepository(canonical) {
		return Workspace{}, errInput
	}
	ctx, cancel := context.WithTimeout(ctx, materializationBudget)
	defer cancel()
	if m.verify(ctx, w) != nil {
		return Workspace{}, errVerification
	}
	if _, err := m.output(ctx, w.Directory, "remote", "set-url", "origin", "https://github.com/"+canonical+".git"); err != nil {
		return Workspace{}, errMaterialize
	}
	w.RepositoryCanonical = canonical
	if m.verify(ctx, w) != nil {
		return Workspace{}, errVerification
	}
	return w, nil
}
