package reviewprocess

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSeparateCLIAuthenticationHomesKeepAllowanceAndExecutionIsolated(t *testing.T) {
	f := newProcessFixture(t)
	firstHome, secondHome := filepath.Join(f.dir, "account-a"), filepath.Join(f.dir, "account-b")
	for _, home := range []string{firstHome, secondHome} {
		if err := os.Mkdir(home, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("CODERABBIT_API_KEY", "synthetic-ambient-key")
	t.Setenv("HOME", firstHome)
	log := filepath.Join(f.dir, "selected-context.log")
	script := fmt.Sprintf(`printf '%%s|%%s|%%s\n' "$HOME" "$1" "${CODERABBIT_API_KEY-unset}" >> %q
if [ "$1" = usage ]; then
  if [ "$HOME" = %q ]; then printf '%%s' %q; else printf '%%s' %q; fi
else
  printf 'review-from:%%s' "$HOME"
fi`, log, firstHome, usageJSON("owner/repo", "owner", 0, 5, 12000, 3600000), usageJSON("owner/repo", "owner", 2, 5, 12000, 3600000))
	executable := f.executable(t, script)
	first, err := New(f.manager, executable, map[string]string{"HOME": firstHome})
	if err != nil {
		t.Fatal(err)
	}
	second, err := New(f.manager, executable, map[string]string{"HOME": secondHome})
	if err != nil {
		t.Fatal(err)
	}
	if usage, err := first.Usage(context.Background(), f.workspace, "owner/repo", "owner"); !errorCode(err, "rate_limited") || usage.Remaining != 0 {
		t.Fatal("exhausted first account was not held", err)
	}
	if _, err := first.Review(context.Background(), f.workspace); !errorCode(err, "permit_required") {
		t.Fatal("exhausted account acquired a review permit", err)
	}
	if _, err := second.Review(context.Background(), f.workspace); !errorCode(err, "permit_required") {
		t.Fatal("first account's check implicitly authorized the second account", err)
	}
	// Selecting another runner is explicit. Its own fresh usage check is still
	// required; no exhausted-account fallback occurs inside either runner.
	if usage, err := second.Usage(context.Background(), f.workspace, "owner/repo", "owner"); err != nil || usage.Remaining != 2 {
		t.Fatal("second account did not read its own allowance", err)
	}
	capture, err := second.Review(context.Background(), f.workspace)
	if err != nil || !capture.Started || capture.ExitCode != 0 || string(capture.Stdout) != "review-from:"+secondHome {
		t.Fatal("explicit second-account execution used the wrong context", err)
	}
	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	want := firstHome + "|usage|unset\n" + secondHome + "|usage|unset\n" + secondHome + "|review|unset\n"
	if string(data) != want || strings.Contains(string(data), "synthetic-ambient-key") {
		t.Fatal("account isolation, command count, or secret environment boundary changed")
	}
}
