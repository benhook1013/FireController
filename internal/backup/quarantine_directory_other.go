//go:build !linux

package backup

import "os"

// Explicit reconciliation is unsupported without descriptor-anchored no-follow
// directory access. Ordinary backup behavior is unchanged.
func openQuarantineDirectory(string) (*os.File, error)       { return nil, invalid() }
func openQuarantineChild(*os.File, string) (*os.File, error) { return nil, invalid() }
