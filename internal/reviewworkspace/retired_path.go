package reviewworkspace

import (
	"errors"
	"os"
	"path/filepath"
)

// RetiredPath observes the manager-owned name of an absent attempt checkout.
// Absence is not settlement or process custody; the native ledger owns eligibility.
func (m *Manager) RetiredPath(attempt string) (string, error) {
	if m == nil || !validID(attempt) || m.layout() != nil {
		return "", errVerification
	}
	path := filepath.Join(m.root, "workspaces", attempt)
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		return "", errVerification
	}
	return path, nil
}
