package profile

import (
	"path/filepath"
	"testing"
)

func TestCredentialReferencesResolveAgainstConfigurationDirectory(t *testing.T) {
	base := t.TempDir()
	config := filepath.Join(base, "configuration", "profiles.json")
	if got := resolveCredential(config, "../credentials/worker.token"); got != filepath.Join(base, "credentials", "worker.token") {
		t.Fatalf("wrong relative reference: %s", got)
	}
	absolute := filepath.Join(base, "private", "worker.token")
	if got := resolveCredential(config, absolute); got != absolute {
		t.Fatalf("wrong absolute reference: %s", got)
	}
}
