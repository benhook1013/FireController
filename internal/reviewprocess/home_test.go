package reviewprocess

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestIsolatedHomesKeepRunsApartAndKeepLogin(t *testing.T) {
	f := newProcessFixture(t)
	shared := filepath.Join(f.dir, "shared-home")
	if err := os.MkdirAll(filepath.Join(shared, ".coderabbit"), 0o700); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"auth.json": `{"token":"old"}`, "machine-id": "machine", "stats.json": `{"shared":true}`} {
		if err := os.WriteFile(filepath.Join(shared, ".coderabbit", name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	seen := filepath.Join(f.dir, "seen")
	if err := os.Mkdir(seen, 0o700); err != nil {
		t.Fatal(err)
	}
	// Each run sees copied login files only, writes its own stats and refreshes login.
	script := fmt.Sprintf(`test "$(cat "$HOME/.coderabbit/auth.json")" = '{"token":"old"}' -o "$(cat "$HOME/.coderabbit/auth.json")" = '{"token":"new"}' || exit 9
test -f "$HOME/.coderabbit/machine-id" || exit 9
test ! -e "$HOME/.coderabbit/stats.json" || exit 9
case "$XDG_CACHE_HOME" in "$HOME"/*) ;; *) exit 9;; esac
printf '%%s' "$HOME" > %q/$$
printf '{"run":true}' > "$HOME/.coderabbit/stats.json"
printf '{"token":"new"}' > "$HOME/.coderabbit/auth.json"
printf '%%s' %q`, seen, usageJSON("owner/repo", "owner", 4, 5, 12000, 3600000))
	r, err := New(f.manager, f.executable(t, script), map[string]string{"HOME": shared, "XDG_CACHE_HOME": filepath.Join(shared, ".cache")}, WithIsolatedHomes(filepath.Join(f.dir, "homes")))
	if err != nil {
		t.Fatal(err)
	}
	// Same-workspace usage is fenced to the latest query, so run in sequence.
	for range 3 {
		if _, err := r.Usage(context.Background(), f.workspace, "owner/repo", "owner"); err != nil {
			t.Fatal(err)
		}
	}
	homes, _ := os.ReadDir(seen)
	distinct := map[string]bool{}
	for _, h := range homes {
		data, _ := os.ReadFile(filepath.Join(seen, h.Name()))
		distinct[string(data)] = true
		if _, err := os.Stat(string(data)); !os.IsNotExist(err) {
			t.Fatal("run home not removed", string(data))
		}
	}
	if len(distinct) != 3 {
		t.Fatal("runs shared a home", distinct)
	}
	if data, _ := os.ReadFile(filepath.Join(shared, ".coderabbit", "stats.json")); string(data) != `{"shared":true}` {
		t.Fatal("shared stats touched", string(data))
	}
	if data, _ := os.ReadFile(filepath.Join(shared, ".coderabbit", "auth.json")); string(data) != `{"token":"new"}` {
		t.Fatal("refreshed login lost", string(data))
	}
}

func TestIsolatedHomesRequireConfiguredHome(t *testing.T) {
	f := newProcessFixture(t)
	if _, err := New(f.manager, f.executable(t, "exit 0"), nil, WithIsolatedHomes(filepath.Join(f.dir, "homes"))); !errorCode(err, "configuration_invalid") {
		t.Fatal("isolation without a HOME accepted", err)
	}
}
