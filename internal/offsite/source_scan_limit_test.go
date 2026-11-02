package offsite

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestOffsiteSourceBundleLimitDoesNotCountPublicationStaging(t *testing.T) {
	root := filepath.Join(t.TempDir(), "private-root")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	// These are enumeration candidates only, not fabricated valid bundles.
	// Full bundle verification remains owned by newestEntries and backup.Inspect.
	for index := range maxSourceBundles {
		if err := os.Mkdir(filepath.Join(root, fmt.Sprintf("backup_%032x", index)), 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(root, ".backup_ffffffffffffffffffffffffffffffff.pending"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "README"), []byte("Unrelated retained file"), 0600); err != nil {
		t.Fatal(err)
	}
	entries, err := sourceBundleEntries(t.Context(), Config{SourceRoot: root})
	if err != nil || len(entries) != maxSourceBundles {
		t.Fatalf("publication staging consumed the supported bundle allowance: %d %#v", len(entries), err)
	}
	if err := os.Mkdir(filepath.Join(root, fmt.Sprintf("backup_%032x", maxSourceBundles)), 0700); err != nil {
		t.Fatal(err)
	}
	_, err = sourceBundleEntries(t.Context(), Config{SourceRoot: root})
	wantCode(t, err, "offsite_source_limit")
}

func TestOffsiteNewestIgnoresStagingAndVerifiesActualBundle(t *testing.T) {
	f := newFixture(t)
	bundle := f.bundle(t)
	staging := filepath.Join(f.c.SourceRoot, "."+bundle.ID+".pending")
	if err := os.Mkdir(staging, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(staging, "unfinished"), []byte("Unpublished backup bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.c.SourceRoot, "unrelated.txt"), []byte("Keep this file"), 0600); err != nil {
		t.Fatal(err)
	}
	before := sourceSelectionBytes(t, f.c.SourceRoot)
	selected, err := newest(t.Context(), f.c)
	if err != nil || selected.ID != bundle.ID || selected.State != "verified" {
		t.Fatal("staging prevented selection of the actual verified source", selected, err)
	}
	if !reflect.DeepEqual(before, sourceSelectionBytes(t, f.c.SourceRoot)) {
		t.Fatal("source discovery modified a backup or unrelated staging")
	}
}

func TestOffsiteSourceRootEnumerationRemainsBounded(t *testing.T) {
	root := filepath.Join(t.TempDir(), "private-root")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	for index := range maxSourceRootEntries {
		if err := os.WriteFile(filepath.Join(root, fmt.Sprintf("unrelated-%04d", index)), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := sourceBundleEntries(t.Context(), Config{SourceRoot: root})
	if err != nil || len(entries) != 0 {
		t.Fatalf("exact root enumeration bound refused: %d %#v", len(entries), err)
	}
	if err := os.WriteFile(filepath.Join(root, "one-over-root-limit"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	_, err = sourceBundleEntries(t.Context(), Config{SourceRoot: root})
	wantCode(t, err, "offsite_source_limit")
}
