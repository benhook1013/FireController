package profile

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func setupTestDirectory(t *testing.T) string {
	t.Helper()
	path := t.TempDir()
	if err := os.Chmod(path, 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func setupTestBinding() Binding {
	return Binding{Context: Context{Profile: "builder", ServerURL: "https://controller.example.com", InstanceID: "instance-a", ProjectID: "project-a", WorkerID: "worker-a"}, CredentialFile: "credentials/not-created.token"}
}

func requireSetupCode(t *testing.T, err *Error, code string) {
	t.Helper()
	if err == nil || err.Code != code {
		t.Fatalf("error = %#v, want %s", err, code)
	}
}

func TestCreatePrivateProfileAndExactRetry(t *testing.T) {
	root := setupTestDirectory(t)
	path := filepath.Join(root, "new", "agent", "profiles.json")
	binding := setupTestBinding()
	got, err := Create(path, binding)
	if err != nil || got != binding.Context {
		t.Fatalf("create = %#v, %v", got, err)
	}
	loaded, err := LoadBinding(path, binding.Context.Profile)
	if err != nil || loaded.Context != binding.Context || loaded.CredentialFile != filepath.Join(filepath.Dir(path), binding.CredentialFile) {
		t.Fatalf("load = %#v, %v", loaded, err)
	}
	if _, err := os.Stat(loaded.CredentialFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("credential was unexpectedly created or read: %v", err)
	}
	before, _ := os.Stat(path)
	data, _ := os.ReadFile(path)
	if got, err = Create(path, binding); err != nil || got != binding.Context {
		t.Fatalf("retry = %#v, %v", got, err)
	}
	after, _ := os.Stat(path)
	retried, _ := os.ReadFile(path)
	if !os.SameFile(before, after) || !before.ModTime().Equal(after.ModTime()) || !bytes.Equal(data, retried) {
		t.Fatal("exact retry changed the configuration")
	}
	if runtime.GOOS != "windows" {
		parent, _ := os.Stat(filepath.Dir(path))
		if before.Mode().Perm() != 0o600 || parent.Mode().Perm() != 0o700 {
			t.Fatalf("file/parent modes = %v/%v", before.Mode(), parent.Mode())
		}
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 || entries[0].Name() != "profiles.json" {
		t.Fatalf("temporary publication files remain: %v", entries)
	}
}

func TestCreateExistingMultipleProfilesNeverWrites(t *testing.T) {
	path := filepath.Join(setupTestDirectory(t), "profiles.json")
	binding := setupTestBinding()
	// Noncanonical whitespace and another profile must survive a semantic retry.
	data := []byte(`{ "version":1, "profiles": {"other":{"server_url":"http://localhost:8080","instance_id":"i2","project_id":"p2","worker_id":"w2","credential_file":"other.token"}, "builder":{"server_url":"https://controller.example.com","instance_id":"instance-a","project_id":"project-a","worker_id":"worker-a","credential_file":"credentials/not-created.token"}} }` + "\n")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	before, _ := os.Stat(path)
	if _, err := Create(path, binding); err != nil {
		t.Fatal(err)
	}
	changed := binding
	changed.Context.WorkerID = "different"
	_, err := Create(path, changed)
	requireSetupCode(t, err, "profiles_conflict")
	changed = binding
	changed.Context.Profile = "absent"
	_, err = Create(path, changed)
	requireSetupCode(t, err, "profiles_conflict")
	changed = binding
	changed.CredentialFile = "./credentials/not-created.token"
	_, err = Create(path, changed)
	requireSetupCode(t, err, "profiles_conflict")
	after, _ := os.Stat(path)
	actual, _ := os.ReadFile(path)
	if !bytes.Equal(actual, data) || !os.SameFile(before, after) || !before.ModTime().Equal(after.ModTime()) {
		t.Fatal("existing profiles changed")
	}
}

func TestCreateConcurrentCreators(t *testing.T) {
	for _, different := range []bool{false, true} {
		t.Run(map[bool]string{false: "equal", true: "different"}[different], func(t *testing.T) {
			path := filepath.Join(setupTestDirectory(t), "private", "profiles.json")
			const count = 16
			var wg sync.WaitGroup
			results := make(chan Binding, count)
			errors := make(chan *Error, count)
			for i := range count {
				binding := setupTestBinding()
				if different && i%2 == 1 {
					binding.Context.ProjectID = "project-b"
				}
				wg.Add(1)
				go func() {
					defer wg.Done()
					_, err := Create(path, binding)
					if err == nil {
						results <- binding
					} else {
						errors <- err
					}
				}()
			}
			wg.Wait()
			close(results)
			close(errors)
			loaded, err := Load(path, "builder")
			if err != nil {
				t.Fatalf("concurrent publication left unreadable file: %v", err)
			}
			for binding := range results {
				if loaded != binding.Context {
					t.Fatal("different bindings both succeeded")
				}
			}
			for err := range errors {
				if !different || err.Code != "profiles_conflict" {
					t.Fatalf("unexpected concurrent error: %#v", err)
				}
			}
			entries, _ := os.ReadDir(filepath.Dir(path))
			if len(entries) != 1 {
				t.Fatalf("unowned or incomplete files remain: %v", entries)
			}
		})
	}
}

func TestCreateValidatesBeforeFilesystemAndMarshal(t *testing.T) {
	tests := []struct {
		name string
		edit func(*Binding)
		code string
	}{
		{"empty selector", func(b *Binding) { b.Context.Profile = "" }, "profiles_invalid"},
		{"remote http", func(b *Binding) { b.Context.ServerURL = "http://controller.example.com" }, "profiles_unsafe_url"},
		{"whitespace id", func(b *Binding) { b.Context.InstanceID = " instance" }, "profiles_invalid"},
		{"empty reference", func(b *Binding) { b.CredentialFile = "" }, "profiles_invalid"},
		{"oversize", func(b *Binding) { b.Context.ProjectID = strings.Repeat("x", MaxConfigBytes) }, "profiles_too_large"},
	}
	for _, field := range []string{"profile", "url", "instance", "project", "worker", "reference"} {
		tests = append(tests, struct {
			name string
			edit func(*Binding)
			code string
		}{"utf8 " + field, func(b *Binding) {
			bad := "invalid\xfftext"
			switch field {
			case "profile":
				b.Context.Profile = bad
			case "url":
				b.Context.ServerURL = bad
			case "instance":
				b.Context.InstanceID = bad
			case "project":
				b.Context.ProjectID = bad
			case "worker":
				b.Context.WorkerID = bad
			case "reference":
				b.CredentialFile = bad
			}
		}, "profiles_invalid_encoding"})
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := setupTestDirectory(t)
			path := filepath.Join(root, "must-not-exist", "profiles.json")
			binding := setupTestBinding()
			tt.edit(&binding)
			_, err := Create(path, binding)
			requireSetupCode(t, err, tt.code)
			entries, _ := os.ReadDir(root)
			if len(entries) != 0 {
				t.Fatal("invalid input wrote filesystem state")
			}
		})
	}
	_, err := Create("relative/profiles.json", setupTestBinding())
	requireSetupCode(t, err, "profiles_invalid_path")
}

func TestCreatePreservesUnicodeAndLiteralCredentialReference(t *testing.T) {
	path := filepath.Join(setupTestDirectory(t), "profiles.json")
	binding := setupTestBinding()
	binding.Context.Profile = "編集者😀"
	binding.Context.ProjectID = "literal\\u1234-�"
	binding.CredentialFile = "credentials/$(false)-`false`-編集者.token"
	if got, err := Create(path, binding); err != nil || got != binding.Context {
		t.Fatalf("unicode create: %#v %v", got, err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	config, configErr := decodeConfiguration(data)
	if configErr != nil || config.Profiles[binding.Context.Profile].CredentialFile != binding.CredentialFile {
		t.Fatalf("credential reference altered: %#v %v", config, configErr)
	}
}

func TestCreateUnsafeExistingFilesAndDirectories(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix permissions and links")
	}
	for _, kind := range []string{"symlink", "directory", "public file", "public parent", "symlink parent", "blocked parent"} {
		t.Run(kind, func(t *testing.T) {
			root := setupTestDirectory(t)
			path := filepath.Join(root, "profiles.json")
			code := "profiles_not_regular"
			sentinel := filepath.Join(root, "unrelated")
			if err := os.WriteFile(sentinel, []byte("untouched"), 0o600); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "symlink":
				if err := os.Symlink(sentinel, path); err != nil {
					t.Fatal(err)
				}
			case "directory":
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
			case "public file":
				if err := os.WriteFile(path, []byte("untouched"), 0o644); err != nil {
					t.Fatal(err)
				}
				code = "profiles_not_private"
			case "public parent":
				if err := os.Chmod(root, 0o755); err != nil {
					t.Fatal(err)
				}
				code = "profiles_not_private"
			case "symlink parent":
				link := filepath.Join(root, "link")
				if err := os.Symlink(root, link); err != nil {
					t.Fatal(err)
				}
				path = filepath.Join(link, "profiles.json")
				code = "profiles_write_failed"
			case "blocked parent":
				path = filepath.Join(sentinel, "profiles.json")
				code = "profiles_write_failed"
			}
			_, err := Create(path, setupTestBinding())
			requireSetupCode(t, err, code)
			actual, _ := os.ReadFile(sentinel)
			if string(actual) != "untouched" {
				t.Fatal("unrelated file changed")
			}
			if strings.Contains(err.Error(), root) {
				t.Fatal("private path leaked")
			}
		})
	}
}

func TestCreateRejectsFIFOBeforeOpen(t *testing.T) {
	if path := os.Getenv("FC_TEST_CREATE_FIFO"); path != "" {
		_, err := Create(path, setupTestBinding())
		requireSetupCode(t, err, "profiles_not_regular")
		return
	}
	if runtime.GOOS == "windows" {
		t.Skip("Unix FIFO")
	}
	if _, err := exec.LookPath("mkfifo"); err != nil {
		t.Skip("mkfifo unavailable")
	}
	path := filepath.Join(setupTestDirectory(t), "profiles.fifo")
	if out, err := exec.Command("mkfifo", path).CombinedOutput(); err != nil {
		t.Fatalf("mkfifo: %s %v", out, err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, executable, "-test.run=^TestCreateRejectsFIFOBeforeOpen$")
	cmd.Env = append(os.Environ(), "FC_TEST_CREATE_FIFO="+path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("FIFO check: %v %s", err, out)
	}
}

func TestCreatePublishedSyncFailurePreservesRetryableTarget(t *testing.T) {
	path := filepath.Join(setupTestDirectory(t), "profiles.json")
	binding := setupTestBinding()
	data, _ := json.Marshal(configuration{Version: 1, Profiles: map[string]entry{"builder": {ServerURL: binding.Context.ServerURL, InstanceID: binding.Context.InstanceID, ProjectID: binding.Context.ProjectID, WorkerID: binding.Context.WorkerID, CredentialFile: binding.CredentialFile}}})
	err := publishSetupFile(path, data, 0o600, "profiles", func([]byte) *Error { t.Fatal("unexpected existing target"); return nil }, func(string) error { return errors.New("private injected failure") })
	requireSetupCode(t, err, "profiles_write_uncertain")
	if _, err := Load(path, "builder"); err != nil {
		t.Fatalf("published target unavailable: %v", err)
	}
	if _, err := Create(path, binding); err != nil {
		t.Fatalf("exact retry did not recover: %v", err)
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Fatalf("failed temporary file not cleaned: %v", entries)
	}
}
