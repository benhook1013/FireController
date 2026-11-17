package reviewexec

import (
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/githubhook"
)

func TestLifecycleHintAdmissionHoldsScheduleLifetime(t *testing.T) {
	f, s, _ := lifecycleFixture(t)
	attachLifecycleHints(t, f, s)
	// The metadata-only hint skips cached-CI matching, so this mutex stalls only
	// final schedule admission after the read-only database selector completes.
	f.store.DB().SetMaxIdleConns(0)
	before := f.store.DB().Stats().MaxIdleClosed
	s.mu.Lock()
	locked := true
	defer func() {
		if locked {
			s.mu.Unlock()
		}
	}()
	done := make(chan error, 1)
	go func() {
		done <- f.e.HintGitHub(t.Context(), githubhook.Hint{RepositoryID: 801, PullNumbers: []int64{2}, Metadata: true})
	}()
	deadline := time.Now().Add(time.Second)
	for f.store.DB().Stats().MaxIdleClosed == before && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if f.store.DB().Stats().MaxIdleClosed == before {
		s.mu.Unlock()
		locked = false
		<-done
		t.Fatal("hint did not complete database selection")
	}
	// Once the database read is finished, the initial lifetime read lock is gone.
	// Only the final admission fence can prevent replacement while s.mu blocks.
	held := false
	deadline = time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if !f.e.lifecycleMu.TryLock() {
			held = true
			break
		}
		f.e.lifecycleMu.Unlock()
		time.Sleep(time.Millisecond)
	}
	s.mu.Unlock()
	locked = false
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("hint admission did not finish")
	}
	if !held {
		t.Fatal("schedule could be stopped/replaced while hint admission targeted it")
	}
	if s.due[lifecycleKey{attemptProject, 2}].hints[lifecycleMetadata].wanted != 1 {
		t.Fatal("hint was not admitted exactly once")
	}
	// The fence is released after mutation, so a subsequent stop can proceed.
	if !f.e.lifecycleMu.TryLock() {
		t.Fatal("hint leaked lifetime read lock")
	}
	f.e.lifecycleMu.Unlock()
}
