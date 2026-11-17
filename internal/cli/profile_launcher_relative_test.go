package cli_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/benhook1013/FireController/internal/cli"
)

func TestProfileLauncherRejectsRelativeProfilesBeforeFileAccess(t *testing.T) {
	root := setupPrivateDirectory(t)
	path := filepath.Join(root, "profiles.json")
	code, out, stderr := run(profileCreateArgs(path, "builder", "https://example.invalid"), nil)
	setupResult(t, code, out, stderr, "configured", "builder")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for _, selected := range []string{path, filepath.Join(root, "missing.json")} {
		relative, err := filepath.Rel(cwd, selected)
		if err != nil {
			t.Fatal(err)
		}
		output := filepath.Join(root, "must-not-create")
		failure(t, []string{"--profiles-file", relative, "--profile", "builder", "profile", "launcher", "--output", output}, nil, cli.ExitUsage, "invalid_arguments")
		if _, err := os.Lstat(output); !os.IsNotExist(err) {
			t.Fatal("invalid input published a launcher", err)
		}
	}
	after, err := os.ReadFile(path)
	if err != nil || string(before) != string(after) {
		t.Fatal("invalid input changed the profile", err)
	}
}
