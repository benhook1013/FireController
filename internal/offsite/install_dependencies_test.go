package offsite

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestInspectBatchInstallationMissingTransportReadOnly(t *testing.T) {
	path, b, c := preflightConfigFixture(t)
	got, err := InspectBatchInstallation(path)
	if err != nil || got.InstallationReady {
		t.Fatal("inspection", got, err)
	}
	missing := map[string]bool{}
	for _, d := range got.Dependencies {
		if d.State == "missing" {
			missing[d.Role] = true
		}
	}
	for _, role := range []string{"batch_state", "source", "provider_state", "transport_executable", "transport_configuration"} {
		if !missing[role] {
			t.Fatal("missing dependency hidden", role)
		}
	}
	for _, p := range []string{b.StateDir, c.StateDir, c.SourceRoot, c.RcloneBinary, c.RcloneConfig} {
		if _, err := os.Lstat(p); !os.IsNotExist(err) {
			t.Fatal("inspection created dependency", p, err)
		}
	}
}

func TestInspectBatchInstallationCredentialNotParsedOrExecuted(t *testing.T) {
	path, b, c := preflightConfigFixture(t)
	for _, p := range []string{b.StateDir, c.StateDir, c.SourceRoot} {
		if err := os.Mkdir(p, 0700); err != nil {
			t.Fatal(err)
		}
	}
	// Malformed credential content must not be decoded; executable is inert and
	// intentionally invalid. Presence cannot qualify either one's behavior.
	for p, body := range map[string]string{c.RcloneBinary: "never execute this", c.RcloneConfig: "PRIVATE INVALID CREDENTIAL SENTINEL"} {
		if err := os.WriteFile(p, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(c.RcloneBinary, 0700); err != nil {
		t.Fatal(err)
	}
	got, err := InspectBatchInstallation(path)
	if err != nil || got.InstallationReady {
		t.Fatal(got, err)
	}
	for _, d := range got.Dependencies {
		if d.State != "present" {
			t.Fatal(d)
		}
	}
	raw, err := os.ReadFile(c.RcloneConfig)
	if err != nil || string(raw) != "PRIVATE INVALID CREDENTIAL SENTINEL" {
		t.Fatal("credentials changed")
	}
}

func TestInstallationPathStateRefusals(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix mode/alias contract")
	}
	root := t.TempDir()
	file := filepath.Join(root, "file")
	if err := os.WriteFile(file, []byte("inert"), 0644); err != nil {
		t.Fatal(err)
	}
	if got := installationPathState(file, "file", true); got != "not_private" {
		t.Fatal(got)
	}
	if got := installationPathState(file, "executable", false); got != "not_executable" {
		t.Fatal(got)
	}
	if got := installationPathState(root, "file", false); got != "wrong_type" {
		t.Fatal(got)
	}
	alias := filepath.Join(root, "alias")
	if err := os.Symlink(root, alias); err != nil {
		t.Fatal(err)
	}
	if got := installationPathState(filepath.Join(alias, "file"), "file", false); got != "symlink" {
		t.Fatal(got)
	}
}

func TestInspectBatchInstallationUsesCanonicalInvalidity(t *testing.T) {
	path, b, c := preflightConfigFixture(t)
	c.RcloneConfig = filepath.Join(b.StateDir, "credentials")
	savePreflightJSON(t, b.Configs[0], c)
	if _, err := InspectBatchInstallation(path); err == nil {
		t.Fatal("canonical overlap violation bypassed")
	}
}
