package reviewexec

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Scheduling stays generic: no production code may order or prefer work by a
// specific repository identity.
func TestSchedulingHasNoRepositorySpecificPriority(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		body, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(strings.ToLower(string(body)), "benhook1013/firemud") {
			t.Errorf("%s hard-codes a repository in scheduling", name)
		}
	}
}
