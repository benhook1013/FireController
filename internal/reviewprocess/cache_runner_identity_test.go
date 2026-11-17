package reviewprocess

import (
	"path/filepath"
	"testing"
	"time"
)

func TestCacheRunnerConfigurationIdentity(t *testing.T) {
	f := newProcessFixture(t)
	r := f.runner(t, "exit 0")
	home := filepath.Join(f.dir, "private-home")
	auth := map[string]string{"HOME": home}
	if !r.MatchesCacheConfiguration(f.manager, r.executable, auth, 0) {
		t.Fatal("original configuration refused")
	}
	if r.MatchesCacheConfiguration(f.manager, "/bin/true", auth, 0) {
		t.Fatal("different executable admitted")
	}
	if r.MatchesCacheConfiguration(f.manager, r.executable, map[string]string{"HOME": home, "XDG_CACHE_HOME": home}, 0) {
		t.Fatal("same HOME different environment admitted")
	}
	if r.MatchesCacheConfiguration(f.manager, r.executable, auth, time.Second) {
		t.Fatal("different timeout admitted")
	}
	if r.MatchesCacheConfiguration(nil, r.executable, auth, 0) {
		t.Fatal("unknown manager admitted")
	}
}
