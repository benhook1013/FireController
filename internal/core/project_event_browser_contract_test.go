package core

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"slices"
	"testing"
)

// Keep the browser's literal union (and the independent privacy allowlist test)
// while proving the wire vocabularies cannot silently drift across languages.
func TestProjectEventBrowserVocabularyMatchesServer(t *testing.T) {
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate browser contract")
	}
	body, err := os.ReadFile(filepath.Join(filepath.Dir(source), "..", "..", "webapp", "src", "project-event-types.ts"))
	if err != nil {
		t.Fatal(err)
	}
	array := regexp.MustCompile(`(?s)export const projectEventTypes = \[(.*?)\] as const;`).FindSubmatch(body)
	if len(array) != 2 {
		t.Fatal("projectEventTypes must remain an explicit literal array, or update this cross-language contract check")
	}
	literal := regexp.MustCompile(`'([^']+)'\s*,?`)
	matches := literal.FindAllSubmatch(array[1], -1)
	if len(matches) == 0 || len(regexp.MustCompile(`\s+`).ReplaceAll(literal.ReplaceAll(array[1], nil), nil)) != 0 {
		t.Fatal("unrecognized browser event vocabulary syntax")
	}
	browser := make([]string, 0, len(matches))
	seen := map[string]bool{}
	for _, match := range matches {
		name := string(match[1])
		if seen[name] {
			t.Fatalf("duplicate browser event %q", name)
		}
		seen[name] = true
		browser = append(browser, name)
	}
	server := slices.Clone(projectEventTypes[:])
	slices.Sort(server)
	slices.Sort(browser)
	if !reflect.DeepEqual(server, browser) {
		t.Fatalf("project event wire vocabulary drift:\nserver %v\nbrowser %v", server, browser)
	}
}
