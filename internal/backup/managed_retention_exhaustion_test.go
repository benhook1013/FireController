package backup

import (
	"os"
	"path/filepath"
	"testing"
)

func TestManagedRetentionRemovesEmptyCLIExhaustionDirectory(t *testing.T) {
	m, b, _ := retentionCancellationFixture(t)
	path := filepath.Join(b.destination, b.id)
	if err := os.Mkdir(filepath.Join(path, "cli-exhaustion-captures"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := m.retain(t.Context()); err != nil {
		t.Fatal(err)
	}
	if state := retentionBundleState(t, m, b); state != "deleted" {
		t.Fatal(state)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("bundle not removed", err)
	}
}
func TestManagedRetentionNeverRecursivelyDeletesUnknownCLIExhaustionBytes(t *testing.T) {
	m, b, _ := retentionCancellationFixture(t)
	path := filepath.Join(b.destination, b.id, "cli-exhaustion-captures")
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(path, "unexpected.json")
	if err := os.WriteFile(file, []byte("preserve"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := m.retain(t.Context()); err == nil {
		t.Fatal("unexpected artifact was accepted")
	}
	if data, err := os.ReadFile(file); err != nil || string(data) != "preserve" {
		t.Fatal("unknown bytes changed", err)
	}
}
