//go:build linux

package reviewruntime

import (
	"context"
	"encoding/json"
	"github.com/benhook1013/FireController/internal/reviewexec"
	"github.com/benhook1013/FireController/internal/reviewprocess"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestAdoptedVendorCacheNativeNamespace(t *testing.T) {
	if os.Getenv("FC_CACHE_ADOPTED_NAMESPACE") != "1" || os.Geteuid() != 0 {
		t.Skip("explicit root disposable namespace only")
	}
	self, e := os.Readlink("/proc/self/ns/mnt")
	if e != nil {
		t.Fatal(e)
	}
	initial, e := os.Readlink("/proc/1/ns/mnt")
	if e != nil || self == initial {
		t.Fatal("private namespace required", e)
	}
	if e = syscall.Mount("", "/", "", syscall.MS_REC|syscall.MS_PRIVATE, ""); e != nil {
		t.Fatal(e)
	}
	f := newCLIEvidenceFixture(t)
	home := filepath.Join(t.TempDir(), "home")
	if e = os.Mkdir(home, 0700); e != nil {
		t.Fatal(e)
	}
	git, e := exec.LookPath("git")
	if e != nil {
		t.Fatal(e)
	}
	cap := OperatorCapability{ID: "runtime-capability", Account: "runtime-account", Organization: "owner", Channels: []string{"cli", "hosted"}, GitExecutable: git, CLIExecutable: "/bin/true", AuthenticationEnvironment: map[string]string{"HOME": home}}
	cap.Fingerprint, e = capabilityFingerprint(cap)
	if e != nil {
		t.Fatal(e)
	}
	past := time.Now()
	observedNow := past.Add(time.Hour) // Explicit synthetic retention observation, not journal rewriting.
	f.engine, e = reviewexec.New(f.store, []reviewexec.Capability{{ID: cap.ID, Provider: "coderabbit", Account: cap.Account, Channels: cap.Channels, Fingerprint: cap.Fingerprint}})
	if e != nil {
		t.Fatal(e)
	}
	a, w := f.startAdmitted(t, 2)
	capture := makeCompleteCLICapture(t, w, mustChangedFiles(t, f.manager, w))
	if _, e = RetainCLI(t.Context(), f.captures, f.engine, f.manager, a, w, capture, past.Add(-time.Second), past); e != nil {
		t.Fatal(e)
	}
	if _, e = f.engine.Advance(t.Context(), f.project, a.ID, a.Revision, "completed", "completion", ""); e != nil {
		t.Fatal(e)
	}
	if _, e = f.manager.Prune(t.Context(), func(ctx context.Context, id string) (bool, error) {
		return f.engine.WorkspaceDisposable(ctx, cap.ID, id, observedNow)
	}); e != nil {
		t.Fatal(e)
	}
	relative := "0123abcd/4567abcd/reviews/1700000000000"
	session := filepath.Join(home, ".coderabbit", "reviews", relative)
	if e = os.MkdirAll(session, 0700); e != nil {
		t.Fatal(e)
	}
	metadata, _ := json.Marshal(map[string]string{"workingDirectory": w.Directory})
	for name, data := range map[string][]byte{"git.json": metadata, ".session-complete-v2": nil, "incrementalDiff.v2.json": []byte("diff"), "internalState.json": []byte("state"), "unknown-retained": []byte("keep")} {
		if e = os.WriteFile(filepath.Join(session, name), data, 0600); e != nil {
			t.Fatal(e)
		}
	}
	lease, e := reviewprocess.NewCacheHomeLease(home)
	if e != nil {
		t.Fatal(e)
	}
	defer lease.Close()
	runner, e := reviewprocess.New(f.manager, cap.CLIExecutable, cap.AuthenticationEnvironment, reviewprocess.WithCacheCustody(&reviewprocess.CacheCustody{}), reviewprocess.WithCacheHomeLease(lease))
	if e != nil {
		t.Fatal(e)
	}
	root, e := os.MkdirTemp("/run", "fc-cache-native-binding-")
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	if e = os.Chmod(root, 0755); e != nil {
		t.Fatal(e)
	}
	owner, projection := filepath.Join(root, "owner"), filepath.Join(root, "projection")
	for _, p := range []string{owner, projection} {
		if e = os.Mkdir(p, 0755); e != nil {
			t.Fatal(e)
		}
	}
	var st syscall.Stat_t
	if e = syscall.Stat(home, &st); e != nil {
		t.Fatal(e)
	}
	boot, e := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if e != nil {
		t.Fatal(e)
	}
	group, e := os.ReadFile("/proc/self/cgroup")
	if e != nil {
		t.Fatal(e)
	}
	source := strings.Repeat("1", 40) // Explicit synthetic build identity; no serving-source qualification.
	// Same HOME is insufficient: the actual runner must match the supplied
	// capability executable/environment/deadline before any cleanup is admitted.
	for _, mismatch := range []string{"executable", "environment", "timeout"} {
		executable := cap.CLIExecutable
		auth := make(map[string]string)
		for key, value := range cap.AuthenticationEnvironment {
			auth[key] = value
		}
		var options []reviewprocess.Option
		switch mismatch {
		case "executable":
			executable = "/bin/false"
		case "environment":
			auth["XDG_CACHE_HOME"] = home
		case "timeout":
			options = append(options, reviewprocess.WithUsageTimeout(time.Second))
		}
		other, err := reviewprocess.New(f.manager, executable, auth, options...)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = newAdoptedVendorCacheMaintenance(other, f.manager, f.engine, f.captures, cap, filepath.Join(projection, "binding"), source); err == nil || err.Error() != "cache runner configuration mismatch" {
			t.Fatalf("%s accepted or failed elsewhere: %v", mismatch, err)
		}
	}
	doc := map[string]any{"version": 1, "state": "enabled", "policy": "exclusive-home-v1", "boot_id": strings.TrimSpace(string(boot)), "cgroup": strings.TrimSpace(string(group)), "source": source, "configuration_sha256": cap.Fingerprint, "owner_evidence_sha256": strings.Repeat("2", 64), "home": home, "home_device": st.Dev, "home_inode": st.Ino, "home_uid": st.Uid, "home_gid": st.Gid}
	publish := func() {
		t.Helper()
		b, e := json.Marshal(doc)
		if e != nil {
			t.Fatal(e)
		}
		p := filepath.Join(owner, "stage")
		if e = os.WriteFile(p, b, 0644); e != nil {
			t.Fatal(e)
		}
		if e = os.Rename(p, filepath.Join(owner, "binding")); e != nil {
			t.Fatal(e)
		}
	}
	publish()
	if e = syscall.Mount(owner, projection, "", syscall.MS_BIND, ""); e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() {
		if e := syscall.Unmount(projection, 0); e != nil {
			t.Error(e)
		}
	})
	if e = syscall.Mount("", projection, "", syscall.MS_BIND|syscall.MS_REMOUNT|syscall.MS_RDONLY, ""); e != nil {
		t.Fatal(e)
	}
	m, e := newAdoptedVendorCacheMaintenance(runner, f.manager, f.engine, f.captures, cap, filepath.Join(projection, "binding"), source)
	if e != nil {
		t.Fatal(e)
	}
	got, e := m.retireAt(t.Context(), relative, a.ID, 1024, observedNow)
	if e != nil || got.Files != 2 || got.LogicalBytes != 9 {
		t.Fatal(got, e)
	}
	again, e := m.retireAt(t.Context(), relative, a.ID, 1024, observedNow)
	if e != nil || again.Files != 0 {
		t.Fatal(again, e)
	}
	if _, _, e = f.captures.Read(t.Context(), a.ID); e != nil {
		t.Fatal("capture changed", e)
	}
	for _, name := range []string{"git.json", ".session-complete-v2", "unknown-retained"} {
		if _, e = os.Stat(filepath.Join(session, name)); e != nil {
			t.Fatal(name, e)
		}
	}
	if e = os.WriteFile(filepath.Join(session, "internalState.json"), []byte("retained after revoke"), 0600); e != nil {
		t.Fatal(e)
	}
	doc["state"] = "disabled"
	publish()
	got, e = m.retireAt(t.Context(), relative, a.ID, 1024, observedNow)
	if e == nil || got.Files != 0 {
		t.Fatal("revocation admitted deletion", got, e)
	}
	if _, e = os.Stat(filepath.Join(session, "internalState.json")); e != nil {
		t.Fatal("revoked payload removed")
	}
}
