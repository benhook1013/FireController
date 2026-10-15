package reviewruntime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviewprocess"
)

func newTestCaptureStore(t *testing.T) (*CaptureStore, string) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "captures")
	store, err := NewCaptureStore(root)
	if err != nil {
		t.Fatal(err)
	}
	return store, root
}

func requireCaptureError(t *testing.T, err error, code string) {
	t.Helper()
	var failure *core.Error
	if !errors.As(err, &failure) || failure.Code != code {
		t.Fatalf("wanted safe %s, got %v", code, err)
	}
	for _, secret := range []string{"synthetic-private-token", "private-capture-path"} {
		if strings.Contains(fmt.Sprintf("%+v %#v", err, err), secret) {
			t.Fatal("error exposed private material")
		}
	}
}

func TestCaptureRoundTripAndImmutableReference(t *testing.T) {
	store, root := newTestCaptureStore(t)
	if strings.Contains(fmt.Sprintf("%v %#v", store, store), root) {
		t.Fatal("store formatting exposed its path")
	}
	for i, input := range []reviewprocess.Capture{
		{Stdout: []byte{0, 255, 0xc0, '\n', '"'}, Stderr: []byte("synthetic-private-token\x00"), ExitCode: -1, Started: true},
		{Stdout: []byte{}, Stderr: []byte{}, ExitCode: 0, Started: false},
		{Stdout: []byte("failure retained honestly"), Stderr: []byte("error"), ExitCode: 255, Started: true},
	} {
		id := fmt.Sprintf("attempt-%d", i)
		ref, err := store.Save(context.Background(), id, input)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(root, id+".json")
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256(data)
		if ref.SHA256 != hex.EncodeToString(digest[:]) || ref.ID != "review_capture_"+ref.SHA256[:32] || ref.Bytes != int64(len(data)) {
			t.Fatalf("reference does not identify exact envelope bytes: %+v", ref)
		}
		if strings.Contains(fmt.Sprint(ref), root) || strings.Contains(fmt.Sprint(ref), "synthetic-private-token") {
			t.Fatal("reference exposed private material")
		}
		loaded, readRef, err := store.Read(context.Background(), id)
		if err != nil || !reflect.DeepEqual(loaded, input) || readRef != ref {
			t.Fatalf("round trip changed capture or reference: %+v %v", loaded, err)
		}
		before, _ := os.Stat(path)
		again, err := store.Save(context.Background(), id, input)
		after, _ := os.Stat(path)
		if err != nil || again != ref || !os.SameFile(before, after) || !before.ModTime().Equal(after.ModTime()) {
			t.Fatal("exact retry replaced the artifact")
		}
		changed := input
		changed.ExitCode = 1
		_, err = store.Save(context.Background(), id, changed)
		requireCaptureError(t, err, "capture_conflict")
		unchanged, _ := os.ReadFile(path)
		if !bytes.Equal(data, unchanged) {
			t.Fatal("conflict modified immutable bytes")
		}
		if runtime.GOOS != "windows" && after.Mode().Perm() != 0600 {
			t.Fatal("capture is not private")
		}
	}
	entries, _ := os.ReadDir(root)
	if len(entries) != 3 {
		t.Fatalf("temporary files leaked: %d entries", len(entries))
	}
}

func TestCaptureConcurrentNoReplace(t *testing.T) {
	for _, different := range []bool{false, true} {
		t.Run(fmt.Sprint(different), func(t *testing.T) {
			store, root := newTestCaptureStore(t)
			start := make(chan struct{})
			type result struct {
				ref CaptureReference
				err error
			}
			results := make(chan result, 2)
			for i := 0; i < 2; i++ {
				go func(i int) {
					<-start
					output := "same"
					if different {
						output = fmt.Sprint(i)
					}
					ref, err := store.Save(context.Background(), "concurrent", reviewprocess.Capture{Stdout: []byte(output), Started: true})
					results <- result{ref, err}
				}(i)
			}
			close(start)
			a, b := <-results, <-results
			if !different {
				if a.err != nil || b.err != nil || a.ref != b.ref {
					t.Fatalf("same capture lost idempotence: %v %v", a.err, b.err)
				}
			} else {
				if a.err != nil {
					a, b = b, a
				}
				if a.err != nil {
					t.Fatal("no immutable winner")
				}
				requireCaptureError(t, b.err, "capture_conflict")
			}
			_, retained, err := store.Read(context.Background(), "concurrent")
			if err != nil || retained != a.ref {
				t.Fatal("retained artifact differs from winner")
			}
			entries, _ := os.ReadDir(root)
			if len(entries) != 1 {
				t.Fatal("concurrent save left disposable files")
			}
		})
	}
}

func TestCaptureCancellationAndInputBounds(t *testing.T) {
	store, root := newTestCaptureStore(t)
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := store.Save(cancelled, "cancelled", reviewprocess.Capture{})
	requireCaptureError(t, err, "capture_unavailable")
	_, _, err = store.Read(cancelled, "cancelled")
	requireCaptureError(t, err, "capture_unavailable")
	for _, id := range []string{"", "../escape", "nested/file", "a.b", "a b", "é", strings.Repeat("a", 129), "a\x00b"} {
		_, err := store.Save(context.Background(), id, reviewprocess.Capture{})
		requireCaptureError(t, err, "validation_failed")
		_, _, err = store.Read(context.Background(), id)
		requireCaptureError(t, err, "validation_failed")
	}
	for _, input := range []reviewprocess.Capture{
		{Stdout: make([]byte, maxCaptureStdout+1)}, {Stderr: make([]byte, maxCaptureStderr+1)}, {ExitCode: -2},
	} {
		_, err := store.Save(context.Background(), "bad", input)
		requireCaptureError(t, err, "validation_failed")
	}
	entries, _ := os.ReadDir(root)
	if len(entries) != 0 {
		t.Fatal("invalid or cancelled input created artifacts")
	}
	// Exercise the actual byte limits, including base64 expansion on disk.
	input := reviewprocess.Capture{Stdout: bytes.Repeat([]byte{255}, maxCaptureStdout), Stderr: bytes.Repeat([]byte{0}, maxCaptureStderr), ExitCode: -1, Started: true}
	ref, err := store.Save(context.Background(), strings.Repeat("a", 128), input)
	if err != nil || ref.Bytes > maxCaptureFile {
		t.Fatalf("maximum capture rejected: %v", err)
	}
	loaded, _, err := store.Read(context.Background(), strings.Repeat("a", 128))
	if err != nil || !reflect.DeepEqual(loaded, input) {
		t.Fatal("maximum capture did not round trip")
	}
}

func TestCaptureRejectsMalformedRetainedArtifactsWithoutReplacement(t *testing.T) {
	store, root := newTestCaptureStore(t)
	const valid = `{"version":1,"attempt_id":"bad","capture":{"stdout":"","stderr":"","exit_code":0,"started":false}}`
	cases := []string{
		`{`, valid + `{}`, string(append([]byte(valid), 255)), strings.Replace(valid, `"version":1`, `"version":2`, 1),
		strings.Replace(valid, `"version":1`, `"version":1,"version":1`, 1),
		strings.Replace(valid, `"version"`, `"Version"`, 1),
		strings.Replace(valid, `"stdout":""`, `"Stdout":""`, 1),
		strings.Replace(valid, `"stdout":""`, `"stdout":"","Stdout":""`, 1),
		strings.Replace(valid, `"version":1`, `"version":null`, 1),
		strings.Replace(valid, `"attempt_id":"bad"`, `"attempt_id":null`, 1),
		strings.Replace(valid, `"attempt_id":"bad"`, `"attempt_id":"other"`, 1),
		strings.Replace(valid, `"stdout":""`, `"stdout":"\ud800"`, 1),
		strings.Replace(valid, `"stdout":""`, `"stdout":"not base64"`, 1),
		strings.Replace(valid, `"exit_code":0`, `"exit_code":4294967296`, 1),
		strings.Replace(valid, `"capture":{`, `"unknown":true,"capture":{`, 1),
	}
	for _, field := range []string{`"stdout":""`, `"stderr":""`, `"exit_code":0`, `"started":false`} {
		name := strings.SplitN(field, ":", 2)[0]
		cases = append(cases, strings.Replace(valid, field, name+":null", 1))
		missing := strings.Replace(valid, field+",", "", 1)
		if missing == valid {
			missing = strings.Replace(valid, ","+field, "", 1)
		}
		cases = append(cases, missing)
	}
	cases = append(cases, strings.Replace(valid, `,"started":false`, "", 1), strings.Replace(valid, `"capture":{"stdout":"","stderr":"","exit_code":0,"started":false}`, `"capture":null`, 1))
	for i, raw := range cases {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			path := filepath.Join(root, "bad.json")
			if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
				t.Fatal(err)
			}
			before, _ := os.Stat(path)
			_, _, err := store.Read(context.Background(), "bad")
			requireCaptureError(t, err, "capture_unavailable")
			_, err = store.Save(context.Background(), "bad", reviewprocess.Capture{})
			requireCaptureError(t, err, "capture_unavailable")
			after, _ := os.Stat(path)
			got, _ := os.ReadFile(path)
			if !bytes.Equal(got, []byte(raw)) || !os.SameFile(before, after) {
				t.Fatal("corrupt final artifact replaced")
			}
		})
	}
	path := filepath.Join(root, "oversize.json")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(maxCaptureFile + 1); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	_, _, err = store.Read(context.Background(), "oversize")
	requireCaptureError(t, err, "capture_unavailable")
}

func TestCaptureRejectsUnsafeFilesystem(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix symlink and mode fixture proof")
	}
	store, root := newTestCaptureStore(t)
	ref, err := store.Save(context.Background(), "good", reviewprocess.Capture{})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "good.json")
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	_, _, err = store.Read(context.Background(), "good")
	requireCaptureError(t, err, "capture_unavailable")
	_, err = store.Save(context.Background(), "good", reviewprocess.Capture{})
	requireCaptureError(t, err, "capture_unavailable")
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(path, filepath.Join(root, "link.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "directory.json"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"link", "directory"} {
		_, _, err := store.Read(context.Background(), id)
		requireCaptureError(t, err, "capture_unavailable")
		_, err = store.Save(context.Background(), id, reviewprocess.Capture{})
		requireCaptureError(t, err, "capture_unavailable")
	}
	parent := t.TempDir()
	link := filepath.Join(parent, "redirect")
	if err := os.Symlink(root, link); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{link, filepath.Join(link, "nested"), path, ""} {
		_, err := NewCaptureStore(bad)
		requireCaptureError(t, err, "configuration_invalid")
	}
	if err := os.Chmod(root, 0755); err != nil {
		t.Fatal(err)
	}
	_, err = NewCaptureStore(root)
	requireCaptureError(t, err, "configuration_invalid")
	_, _, err = store.Read(context.Background(), "good")
	requireCaptureError(t, err, "capture_unavailable")
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	_, retained, err := store.Read(context.Background(), "good")
	if err != nil || retained != ref {
		t.Fatal("unsafe-path errors modified retained artifact")
	}
}

func TestCaptureNonregularFIFO(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux FIFO proof")
	}
	if root := os.Getenv("FC_CAPTURE_FIFO_FIXTURE"); root != "" {
		store, err := NewCaptureStore(root)
		if err != nil {
			t.Fatal(err)
		}
		_, _, err = store.Read(context.Background(), "fifo")
		requireCaptureError(t, err, "capture_unavailable")
		_, err = store.Save(context.Background(), "fifo", reviewprocess.Capture{})
		requireCaptureError(t, err, "capture_unavailable")
		return
	}
	_, root := newTestCaptureStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if output, err := exec.CommandContext(ctx, "mkfifo", "-m", "600", filepath.Join(root, "fifo.json")).CombinedOutput(); err != nil {
		t.Fatalf("FIFO fixture: %v %s", err, output)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	child := exec.CommandContext(ctx, executable, "-test.run=^TestCaptureNonregularFIFO$", "-test.count=1")
	child.Env = append(os.Environ(), "FC_CAPTURE_FIFO_FIXTURE="+root)
	if output, err := child.CombinedOutput(); err != nil {
		t.Fatalf("nonregular guard failed or exceeded external bound: %v %s", err, output)
	}
}
