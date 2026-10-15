package profile

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

func launcherTestFixture(t *testing.T) (string, string, string, Binding) {
	t.Helper()
	if runtime.GOOS == "windows" || runtime.GOOS == "plan9" {
		t.Skip("POSIX launcher")
	}
	root := setupTestDirectory(t)
	profiles := filepath.Join(root, "profiles ' $(false) `false`\n literal.json")
	binding := setupTestBinding()
	binding.Context.Profile = "builder ' $(false) `false`\n literal"
	if _, err := Create(profiles, binding); err != nil {
		t.Fatal(err)
	}
	executable := filepath.Join(root, "fc ' $(false) `false`\n literal")
	if err := os.WriteFile(executable, []byte("#!/bin/sh\nprintf '%s\\000' \"$@\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(root, "bin", "launcher"), profiles, executable, binding
}

func TestCreateLauncherExecutesLiteralFixedAndForwardedArguments(t *testing.T) {
	output, profiles, executable, binding := launcherTestFixture(t)
	got, err := CreateLauncher(output, profiles, binding.Context.Profile, executable)
	if err != nil || got != binding.Context {
		t.Fatalf("launcher = %#v %v", got, err)
	}
	args := []string{"jobs", "text with spaces", "apostrophe's", "$(exit 91)", "`exit 92`", "line\nbreak", "", "*"}
	cmd := exec.Command(output, args...)
	cmd.Env = append(os.Environ(), "FC_PROFILE=unrelated")
	actual, runErr := cmd.Output()
	if runErr != nil {
		t.Fatal(runErr)
	}
	want := append([]string{"--profiles-file", profiles, "--profile", binding.Context.Profile}, args...)
	expected := []byte(strings.Join(want, "\x00") + "\x00")
	if !bytes.Equal(actual, expected) {
		t.Fatalf("literal arguments differ: got %q want %q", actual, expected)
	}
	body, _ := os.ReadFile(output)
	if !bytes.HasPrefix(body, []byte("#!/bin/sh\nexec ")) || !bytes.HasSuffix(body, []byte(" \"$@\"\n")) {
		t.Fatalf("unexpected launcher framing: %q", body)
	}
	before, _ := os.Stat(output)
	if before.Mode().Perm() != 0o700 {
		t.Fatalf("launcher mode: %v", before.Mode())
	}
	if _, err := CreateLauncher(output, profiles, binding.Context.Profile, executable); err != nil {
		t.Fatal(err)
	}
	after, _ := os.Stat(output)
	if !os.SameFile(before, after) || !before.ModTime().Equal(after.ModTime()) {
		t.Fatal("launcher retry wrote existing file")
	}
}

func TestCreateLauncherConcurrentAndConflictingTargets(t *testing.T) {
	output, profiles, executable, binding := launcherTestFixture(t)
	var wg sync.WaitGroup
	errors := make(chan *Error, 12)
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := CreateLauncher(output, profiles, binding.Context.Profile, executable)
			errors <- err
		}()
	}
	wg.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	before, _ := os.ReadFile(output)
	other := filepath.Join(filepath.Dir(executable), "other-fc")
	if err := os.WriteFile(other, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	_, err := CreateLauncher(output, profiles, binding.Context.Profile, other)
	requireSetupCode(t, err, "launcher_conflict")
	after, _ := os.ReadFile(output)
	if !bytes.Equal(before, after) {
		t.Fatal("conflicting launcher overwrote original")
	}
	entries, _ := os.ReadDir(filepath.Dir(output))
	if len(entries) != 1 {
		t.Fatalf("unexpected temporary files: %v", entries)
	}
}

func TestCreateLauncherInvalidInputsDoNotPublish(t *testing.T) {
	for _, kind := range []string{"relative output", "relative profiles", "relative executable", "invalid UTF8 path", "missing profile", "nonexecutable", "directory executable", "symlink target", "nonprivate target", "nonexecutable target"} {
		t.Run(kind, func(t *testing.T) {
			output, profiles, executable, binding := launcherTestFixture(t)
			code := "launcher_invalid_path"
			name := binding.Context.Profile
			switch kind {
			case "relative output":
				output = "relative-launcher"
			case "relative profiles":
				profiles = "relative-profiles"
			case "relative executable":
				executable = "relative-fc"
			case "invalid UTF8 path":
				output += "\xff"
			case "missing profile":
				name = "absent"
				code = "profile_not_found"
			case "nonexecutable":
				if err := os.Chmod(executable, 0o600); err != nil {
					t.Fatal(err)
				}
				code = "launcher_invalid_executable"
			case "directory executable":
				executable = filepath.Dir(executable)
				code = "launcher_invalid_executable"
			default:
				if err := os.Mkdir(filepath.Dir(output), 0o700); err != nil {
					t.Fatal(err)
				}
				if kind == "symlink target" {
					if err := os.Symlink(executable, output); err != nil {
						t.Fatal(err)
					}
					code = "launcher_not_regular"
				} else {
					mode := os.FileMode(0o755)
					code = "launcher_not_private"
					if kind == "nonexecutable target" {
						mode = 0o600
						code = "launcher_invalid_executable"
					}
					if err := os.WriteFile(output, []byte("untouched"), mode); err != nil {
						t.Fatal(err)
					}
				}
			}
			_, err := CreateLauncher(output, profiles, name, executable)
			requireSetupCode(t, err, code)
			if kind == "nonprivate target" || kind == "nonexecutable target" {
				body, _ := os.ReadFile(output)
				if string(body) != "untouched" {
					t.Fatal("unsafe target changed")
				}
			} else if !strings.HasSuffix(kind, "target") {
				if _, err := os.Stat(filepath.Join(filepath.Dir(executable), "bin")); !errors.Is(err, os.ErrNotExist) && kind != "directory executable" {
					t.Fatal("invalid input created launcher parent")
				}
			}
		})
	}
}

func TestCreateLauncherPublishedSyncFailurePreservesExactRetry(t *testing.T) {
	output, profiles, executable, binding := launcherTestFixture(t)
	body := []byte("#!/bin/sh\nexec " + shellLiteral(executable) + " --profiles-file " + shellLiteral(profiles) + " --profile " + shellLiteral(binding.Context.Profile) + " \"$@\"\n")
	err := publishSetupFile(output, body, 0o700, "launcher", func([]byte) *Error { t.Fatal("unexpected existing target"); return nil }, func(string) error { return errors.New("injected directory sync failure") })
	requireSetupCode(t, err, "launcher_write_uncertain")
	if _, err := CreateLauncher(output, profiles, binding.Context.Profile, executable); err != nil {
		t.Fatalf("exact retry: %v", err)
	}
	if out, err := exec.Command(output, "version").Output(); err != nil || !bytes.Contains(out, []byte("version\x00")) {
		t.Fatalf("published launcher unusable: %q %v", out, err)
	}
}

func TestCreateLauncherDifferentConcurrentBindingsNeverOverwrite(t *testing.T) {
	output, profiles, executable, binding := launcherTestFixture(t)
	other := filepath.Join(filepath.Dir(executable), "second executable")
	if err := os.WriteFile(other, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	type result struct {
		executable string
		err        *Error
	}
	results := make(chan result, 16)
	for i := range 16 {
		selected := executable
		if i%2 != 0 {
			selected = other
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := CreateLauncher(output, profiles, binding.Context.Profile, selected)
			results <- result{selected, err}
		}()
	}
	wg.Wait()
	close(results)
	body, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	winner := ""
	for result := range results {
		if result.err != nil {
			requireSetupCode(t, result.err, "launcher_conflict")
			continue
		}
		if winner == "" {
			winner = result.executable
		}
		if winner != result.executable {
			t.Fatal("conflicting concurrent launchers both succeeded")
		}
	}
	if winner == "" || !bytes.Contains(body, []byte(shellLiteral(winner))) {
		t.Fatal("no complete winning launcher")
	}
}

func TestCreateLauncherUnsupportedHostDoesNotWrite(t *testing.T) {
	if runtime.GOOS != "windows" && runtime.GOOS != "plan9" {
		t.Skip("unsupported-host proof")
	}
	root := setupTestDirectory(t)
	_, err := CreateLauncher(filepath.Join(root, "new", "launcher"), filepath.Join(root, "profiles.json"), "builder", filepath.Join(root, "fc"))
	requireSetupCode(t, err, "launcher_unsupported")
	entries, _ := os.ReadDir(root)
	if len(entries) != 0 {
		t.Fatal("unsupported launcher wrote files")
	}
}

func TestCreateLauncherRejectsExecutableWithoutEffectiveAccess(t *testing.T) {
	output, profiles, executable, binding := launcherTestFixture(t)
	if os.Geteuid() == 0 {
		t.Skip("root may execute files with only group or other execute bits")
	}
	// The current user owns the file. A group execute bit cannot compensate
	// for the missing owner execute bit, even when that user belongs to its group.
	if err := os.Chmod(executable, 0o610); err != nil {
		t.Fatal(err)
	}
	_, err := CreateLauncher(output, profiles, binding.Context.Profile, executable)
	requireSetupCode(t, err, "launcher_invalid_executable")
	if _, err := os.Stat(filepath.Dir(output)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("inaccessible executable published launcher state")
	}
	if err := os.Chmod(executable, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := CreateLauncher(output, profiles, binding.Context.Profile, executable); err != nil {
		t.Fatalf("owner-executable target refused: %v", err)
	}
	actual, runErr := exec.Command(output, "version").Output()
	want := []byte(strings.Join([]string{"--profiles-file", profiles, "--profile", binding.Context.Profile, "version"}, "\x00") + "\x00")
	if runErr != nil || !bytes.Equal(actual, want) {
		t.Fatalf("owner-executable launcher failed: %q %v", actual, runErr)
	}
}
