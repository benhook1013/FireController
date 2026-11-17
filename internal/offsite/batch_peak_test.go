package offsite

import (
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Opt-in bounded representative capture-volume test; not a production node
// benchmark. du measures allocated workspace bytes, excluding immutable source.
func TestBatchRepresentativeStagingPeak(t *testing.T) {
	if os.Getenv("FC_TEST_BATCH_PEAK") != "1" || runtime.GOOS == "windows" {
		t.Skip("opt-in disk workload")
	}
	f := newFixture(t)
	f.c.MaxBundleBytes = 2 << 30
	captures := filepath.Join(f.dir, "review-captures")
	if err := os.Mkdir(captures, 0700); err != nil {
		t.Fatal(err)
	}
	rng := rand.New(rand.NewSource(17))
	data := make([]byte, 148212)
	for i := 0; i < 7926; i++ {
		rng.Read(data[:23000])
		if err := os.WriteFile(filepath.Join(captures, fmt.Sprintf("review_attempt_%032x.json", i+1)), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	summary := f.bundle(t)
	root := t.TempDir()
	work := filepath.Join(root, "shared")
	os.Mkdir(work, 0700)
	var peak atomic.Int64
	stop := make(chan struct{})
	done := make(chan struct{})
	sample := func() {
		out, e := exec.Command("du", "-s", "-B1", root).Output()
		if e == nil {
			fields := strings.Fields(string(out))
			if len(fields) > 0 {
				n, _ := strconv.ParseInt(fields[0], 10, 64)
				for old := peak.Load(); n > old; old = peak.Load() {
					if peak.CompareAndSwap(old, n) {
						break
					}
				}
			}
		}
	}
	go func() {
		defer close(done)
		tick := time.NewTicker(25 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-stop:
				sample()
				return
			case <-tick.C:
				sample()
			}
		}
	}()
	defer func() { close(stop); <-done }()
	p := &batchPreparation{work: work}
	archive := filepath.Join(root, "pending.tar.gz")
	r, e := p.linkArchive(t.Context(), summary, archive, f.c)
	if e != nil {
		t.Fatal(e)
	}
	remote := memory()
	body, e := os.ReadFile(archive)
	if e != nil {
		t.Fatal(e)
	}
	provider := filepath.Join(root, "provider")
	os.Mkdir(provider, 0700)
	s := &session{c: f.c, work: provider, transport: remote, batch: p}
	remote.files[s.object(r, "archive.tar.gz")] = body
	for i := 0; i < 3; i++ {
		if e = s.verifyRemote(t.Context(), r); e != nil {
			t.Fatal(e)
		}
	}
	sample()
	t.Logf("captures=7926 payload=%d archive=%d sampled_allocated_staging_peak=%d interval=25ms; transient peak may be missed", int64(len(data))*7926, r.ArchiveBytes, peak.Load())
}
