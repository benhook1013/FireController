package offsite

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/backup"
	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/server"
)

type memoryTransport struct {
	mu               sync.Mutex
	files            map[string][]byte
	puts             map[string]int
	deletes          int
	uncertainReceipt bool
	corruptDownload  bool
	failDelete       bool
	block            chan struct{}
}

func memory() *memoryTransport {
	return &memoryTransport{files: map[string][]byte{}, puts: map[string]int{}}
}
func (m *memoryTransport) read(_ context.Context, name string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.files[name]
	if !ok {
		return nil, notFound
	}
	return bytes.Clone(v), nil
}
func (m *memoryTransport) put(ctx context.Context, source, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.block != nil {
		close(m.block)
		m.block = nil
		<-ctx.Done()
		return failure("offsite_cancelled")
	}
	b, err := os.ReadFile(source)
	if err != nil {
		return err
	}
	m.files[name] = b
	m.puts[name]++
	if m.uncertainReceipt && strings.HasSuffix(name, "receipt.json") {
		m.uncertainReceipt = false
		return failure("offsite_transport_failed")
	}
	return nil
}
func (m *memoryTransport) get(_ context.Context, name, dest string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.files[name]
	if !ok {
		return notFound
	}
	v = bytes.Clone(v)
	if m.corruptDownload && len(v) > 0 {
		v[len(v)/2] ^= 0xff
	}
	return os.WriteFile(dest, v, 0600)
}
func (m *memoryTransport) remove(_ context.Context, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failDelete {
		return failure("offsite_transport_failed")
	}
	delete(m.files, name)
	m.deletes++
	return nil
}

type fixture struct {
	c       Config
	store   *core.Store
	dir     string
	handler http.Handler
	token   string
	count   int
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "source")
	store, err := core.Open(t.Context(), dir)
	if err != nil {
		t.Fatalf("error=%#v", err)
	}
	t.Cleanup(func() { store.Close() })
	token, err := os.ReadFile(filepath.Join(dir, "bootstrap-admin.token"))
	if err != nil {
		t.Fatalf("error=%#v", err)
	}
	f := &fixture{store: store, dir: dir, token: strings.TrimSpace(string(token))}
	f.handler = server.New(store, server.WithRepositoryResolver(func(_ context.Context, name string) (server.RepositoryIdentity, error) {
		return server.RepositoryIdentity{ID: int64(f.count), FullName: name}, nil
	}))
	f.c = Config{Version: 1, SourceRoot: filepath.Join(root, "backups"), InstanceID: store.InstanceID(), StateDir: filepath.Join(root, "transfer"), RcloneBinary: filepath.Join(root, "rclone"), RcloneConfig: filepath.Join(root, "rclone.conf"), Remote: "cloud:firecontroller", Keep: 1, Timeout: "5s", MaxBundleBytes: 16 << 20}
	return f
}
func (f *fixture) addProject(t *testing.T) {
	t.Helper()
	f.count++
	body, _ := json.Marshal(map[string]string{"name": "Project", "repository": "fixture/project" + strings.Repeat("x", f.count)})
	r := httptest.NewRequest("POST", "/api/v1/projects", bytes.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+f.token)
	r.Header.Set("X-FC-Instance", f.c.InstanceID)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Idempotency-Key", "fixture-project-"+strings.Repeat("x", f.count))
	out := httptest.NewRecorder()
	f.handler.ServeHTTP(out, r)
	if out.Code != 200 {
		t.Fatal(out.Code, out.Body.String())
	}
}
func (f *fixture) bundle(t *testing.T) backup.Summary {
	t.Helper()
	f.addProject(t)
	s, err := backup.Create(t.Context(), f.store, f.dir, f.c.SourceRoot)
	if err != nil {
		t.Fatalf("error=%#v", err)
	}
	return s
}
func wantCode(t *testing.T, err error, code string) {
	t.Helper()
	var e *core.Error
	if !errors.As(err, &e) || e.Code != code {
		t.Fatalf("wanted %s, got %#v", code, err)
	}
}
func state(t *testing.T, c Config) Status {
	t.Helper()
	s, e := readState(c)
	if e != nil {
		t.Fatal(e)
	}
	return s
}

func TestOffsiteCompleteIdempotentFetchAndInactiveRestore(t *testing.T) {
	f := newFixture(t)
	bundle := f.bundle(t)
	m := memory()
	result, err := push(t.Context(), f.c, m)
	if err != nil || result.State != "complete" || result.BackupID != bundle.ID {
		t.Fatalf("result=%+v error=%#v", result, err)
	}
	before := len(m.puts)
	again, err := push(t.Context(), f.c, m)
	if err != nil || again.State != "unchanged" || len(m.puts) != before {
		t.Fatal(again, err)
	}
	st := state(t, f.c)
	if st.Pending != nil || st.LastSuccess == nil || len(st.Owned) != 1 || st.LastAttempt.State != "unchanged" {
		t.Fatal(st)
	}
	// Disaster recovery does not depend on the original DB, bundle or ledger.
	if err = f.store.Close(); err != nil {
		t.Fatalf("error=%#v", err)
	}
	if err = os.RemoveAll(f.dir); err != nil {
		t.Fatalf("error=%#v", err)
	}
	if err = os.RemoveAll(f.c.SourceRoot); err != nil {
		t.Fatalf("error=%#v", err)
	}
	fresh := f.c
	fresh.StateDir = filepath.Join(t.TempDir(), "new-state")
	s, err := openSession(fresh, m)
	if err != nil {
		t.Fatalf("error=%#v", err)
	}
	defer s.close()
	destination := filepath.Join(t.TempDir(), "download")
	got, err := s.fetch(t.Context(), "", destination)
	if err != nil || got.ID != bundle.ID || got.State != "verified" {
		t.Fatal(got, err)
	}
	_, err = s.fetch(t.Context(), bundle.ID, destination)
	wantCode(t, err, "offsite_destination_exists")
	restored, err := backup.Restore(t.Context(), got.Path, filepath.Join(t.TempDir(), "restore"))
	if err != nil || restored.State != "inactive" || restored.InstanceID == f.c.InstanceID {
		t.Fatal(restored, err)
	}
	store, err := core.Open(t.Context(), restored.Destination)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	wantCode(t, backup.StartupGuard(t.Context(), store), "restore_inactive")
}

func TestOffsiteUncertainPublicationRecoversSamePendingWithoutSource(t *testing.T) {
	f := newFixture(t)
	f.bundle(t)
	m := memory()
	if _, err := push(t.Context(), f.c, m); err != nil {
		t.Fatalf("error=%#v", err)
	}
	previous := *state(t, f.c).LastSuccess
	latest := f.bundle(t)
	m.uncertainReceipt = true
	_, err := push(t.Context(), f.c, m)
	wantCode(t, err, "offsite_transport_failed")
	st := state(t, f.c)
	if st.Pending == nil || st.Pending.ID != latest.ID || *st.LastSuccess != previous || st.LastAttempt.State != "failed" {
		t.Fatal(st)
	}
	if err = os.RemoveAll(latest.Path); err != nil {
		t.Fatalf("error=%#v", err)
	}
	got, err := push(t.Context(), f.c, m)
	if err != nil || got.BackupID != latest.ID {
		t.Fatal(got, err)
	}
	st = state(t, f.c)
	if st.Pending != nil || st.LastSuccess.ID != latest.ID || len(st.Owned) != 1 {
		t.Fatal(st)
	}
	for name, n := range m.puts {
		if strings.Contains(name, latest.ID) && n != 1 {
			t.Fatal("reposted committed object", name, n)
		}
	}
	if len(m.files) != 3 {
		t.Fatal("old owned objects not pruned", len(m.files))
	}
}

func TestOffsiteCorruptDownloadCannotPublishOrPrune(t *testing.T) {
	f := newFixture(t)
	first := f.bundle(t)
	m := memory()
	if _, err := push(t.Context(), f.c, m); err != nil {
		t.Fatalf("error=%#v", err)
	}
	f.bundle(t)
	m.corruptDownload = true
	_, err := push(t.Context(), f.c, m)
	wantCode(t, err, "offsite_archive_invalid")
	st := state(t, f.c)
	if st.LastSuccess.ID != first.ID || st.Pending == nil || m.deletes != 0 {
		t.Fatal(st, m.deletes)
	}
	for name := range m.files {
		if strings.HasSuffix(name, "receipt.json") && !strings.Contains(name, first.ID) {
			t.Fatal("corrupt copy marked complete")
		}
	}
	m.corruptDownload = false
	if _, err = push(t.Context(), f.c, m); err != nil {
		t.Fatalf("error=%#v", err)
	}
}

func TestOffsiteRetentionFailurePreservesSuccessAndRetriesBeforeUpload(t *testing.T) {
	f := newFixture(t)
	f.bundle(t)
	m := memory()
	if _, err := push(t.Context(), f.c, m); err != nil {
		t.Fatalf("error=%#v", err)
	}
	latest := f.bundle(t)
	m.failDelete = true
	_, err := push(t.Context(), f.c, m)
	wantCode(t, err, "offsite_transport_failed")
	st := state(t, f.c)
	if st.LastSuccess.ID != latest.ID || st.RetentionReason != "offsite_retention_failed" || len(st.Owned) != 2 {
		t.Fatal(st)
	}
	m.corruptDownload = true
	m.failDelete = false
	before := m.deletes
	_, err = push(t.Context(), f.c, m)
	wantCode(t, err, "offsite_archive_invalid")
	if m.deletes != before {
		t.Fatal("pruned without a valid keeper")
	}
	m.corruptDownload = false
	puts := len(m.puts)
	r, err := push(t.Context(), f.c, m)
	if err != nil || r.State != "unchanged" || len(state(t, f.c).Owned) != 1 || len(m.puts) != puts {
		t.Fatal(r, err)
	}
}

func TestOffsiteUnknownOrChangedRemoteObjectsNeverPruned(t *testing.T) {
	f := newFixture(t)
	first := f.bundle(t)
	m := memory()
	if _, err := push(t.Context(), f.c, m); err != nil {
		t.Fatalf("error=%#v", err)
	}
	m.files[f.c.Remote+"/unrelated-important-object"] = []byte("preserve")
	old := f.c.Remote + "/" + f.c.InstanceID + "/" + first.ID + "/archive.tar.gz"
	m.files[old] = []byte("changed foreign data")
	f.bundle(t)
	_, err := push(t.Context(), f.c, m)
	wantCode(t, err, "offsite_remote_conflict")
	if m.deletes != 0 || string(m.files[old]) != "changed foreign data" || string(m.files[f.c.Remote+"/unrelated-important-object"]) != "preserve" {
		t.Fatal("deleted unproven objects")
	}
}

func TestOffsitePendingCancellationAndExclusiveWorkerLeaveCoreFree(t *testing.T) {
	f := newFixture(t)
	f.bundle(t)
	m := memory()
	entered := make(chan struct{})
	m.block = entered
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := push(ctx, f.c, m); done <- err }()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("transfer never started")
	}
	_, err := push(t.Context(), f.c, m)
	wantCode(t, err, "offsite_busy")
	f.addProject(t)
	cancel()
	select {
	case err := <-done:
		wantCode(t, err, "offsite_cancelled")
	case <-time.After(2 * time.Second):
		t.Fatal("cancellation did not join")
	}
	if state(t, f.c).LastSuccess != nil || state(t, f.c).Pending == nil {
		t.Fatal("cancellation invented completion")
	}
	if _, err = push(t.Context(), f.c, m); err != nil {
		t.Fatalf("error=%#v", err)
	}
}

func TestOffsiteDeterministicArchiveAndSourceIsolation(t *testing.T) {
	f := newFixture(t)
	s := f.bundle(t)
	a, err := pack(t.Context(), s.Path, filepath.Join(t.TempDir(), "one.gz"), f.c)
	if err != nil {
		t.Fatalf("error=%#v", err)
	}
	b, err := pack(t.Context(), s.Path, filepath.Join(t.TempDir(), "two.gz"), f.c)
	if err != nil || a != b {
		t.Fatal("archive not deterministic", a, b, err)
	}
	c := f.c
	c.InstanceID = "instance_" + strings.Repeat("f", 32)
	_, err = push(t.Context(), c, memory())
	wantCode(t, err, "offsite_source_empty")
}

func TestOffsiteInterruptedPreparationAndOwnedWorkspaceRecovery(t *testing.T) {
	for _, renamed := range []bool{false, true} {
		t.Run(map[bool]string{false: "before_rename", true: "after_rename"}[renamed], func(t *testing.T) {
			f := newFixture(t)
			bundle := f.bundle(t)
			if err := privateDir(f.c.StateDir, true); err != nil {
				t.Fatal(err)
			}
			work, err := prepareWork(f.c)
			if err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(filepath.Join(work, "interrupted-download"), []byte("disposable transfer bytes"), 0600); err != nil {
				t.Fatal(err)
			}
			prepared := filepath.Join(f.c.StateDir, "prepare.tar.gz")
			receipt, err := pack(t.Context(), bundle.Path, prepared, f.c)
			if err != nil {
				t.Fatal(err)
			}
			s := state(t, f.c)
			s.Pending = &receipt
			if err = writeJSON(filepath.Join(f.c.StateDir, "offsite.json"), s); err != nil {
				t.Fatal(err)
			}
			if renamed {
				if err = os.Rename(prepared, filepath.Join(f.c.StateDir, "pending.tar.gz")); err != nil {
					t.Fatal(err)
				}
			}
			if err = os.RemoveAll(f.c.SourceRoot); err != nil {
				t.Fatal(err)
			}
			remote := memory()
			result, err := push(t.Context(), f.c, remote)
			if err != nil || result.BackupID != bundle.ID || result.State != "complete" {
				t.Fatalf("result=%+v error=%#v", result, err)
			}
			if _, err = os.Lstat(work); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("workspace survived completion: %v", err)
			}
			if state(t, f.c).Pending != nil {
				t.Fatal("pending publication not cleared")
			}
		})
	}
}

func TestOffsiteWorkspaceRefusesUnownedDataAndLinks(t *testing.T) {
	for _, kind := range []string{"unowned", "wrong_marker", "symlink", "empty"} {
		t.Run(kind, func(t *testing.T) {
			f := newFixture(t)
			if err := privateDir(f.c.StateDir, true); err != nil {
				t.Fatal(err)
			}
			dir := filepath.Join(f.c.StateDir, "work")
			outside := t.TempDir()
			if kind == "symlink" {
				if err := os.Symlink(outside, dir); err != nil {
					t.Skip(err)
				}
			} else {
				if err := os.Mkdir(dir, 0700); err != nil {
					t.Fatal(err)
				}
				if kind != "empty" {
					if err := os.WriteFile(filepath.Join(dir, "keep"), []byte("unrecognized"), 0600); err != nil {
						t.Fatal(err)
					}
				}
				if kind == "wrong_marker" {
					if err := os.WriteFile(filepath.Join(dir, ".owner"), []byte("different binding"), 0600); err != nil {
						t.Fatal(err)
					}
				}
			}
			work, err := prepareWork(f.c)
			if kind == "empty" {
				if err != nil {
					t.Fatal(err)
				}
				if err = os.RemoveAll(work); err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil {
				t.Fatal("claimed an unowned workspace")
			}
			if kind == "symlink" {
				if _, err = os.Stat(outside); err != nil {
					t.Fatal("removed target")
				}
			} else if b, err := os.ReadFile(filepath.Join(dir, "keep")); err != nil || string(b) != "unrecognized" {
				t.Fatal("removed unowned data")
			}
		})
	}
}
