package backup

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// The uploader discovers only these exact final backup_ID names. The existing
// clock seam pauses the real producer after reservation, before snapshot I/O.
func TestBundlePublicationHidesUnsealedReservation(t *testing.T) {
	f := newRestoreFixture(t)
	root := filepath.Join(t.TempDir(), "bundles")
	prior, err := Create(t.Context(), f.store, f.dir, root)
	if err != nil {
		t.Fatal(err)
	}
	priorBytes := restoreTree(t, prior.Path)
	sourceRows := backupRows(t, f.store.DB())
	var stage string
	var owned os.FileInfo
	anchor := time.Now()
	now := func() time.Time {
		entries, err := os.ReadDir(root)
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			if backupID.MatchString(entry.Name()) {
				if entry.Name() != prior.ID {
					t.Errorf("unsealed producer appeared in final backup discovery: %s", entry.Name())
					continue
				}
				if _, err := Inspect(t.Context(), filepath.Join(root, entry.Name())); err != nil {
					t.Fatal("existing published backup became unavailable", err)
				}
				continue
			}
			name := strings.TrimSuffix(strings.TrimPrefix(entry.Name(), "."), ".pending")
			if !backupID.MatchString(name) || entry.Name() != "."+name+".pending" {
				t.Fatalf("unexpected producer entry %s", entry.Name())
			}
			stage = filepath.Join(root, entry.Name())
			if err := os.Mkdir(stage, 0700); !os.IsExist(err) {
				t.Fatal("same-ID creator did not share exclusive reservation", err)
			}
			owned, err = os.Lstat(stage)
			if err != nil || !owned.IsDir() {
				t.Fatal("missing private staging reservation", err)
			}
		}
		if stage == "" {
			t.Error("producer did not reserve hidden staging")
		}
		return anchor
	}
	created, err := createBundleWithClock(t.Context(), f.store.DB(), f.store.InstanceID(), f.dir, root, now, func(time.Time) time.Duration { return time.Millisecond })
	if err != nil {
		t.Fatal(err)
	}
	if stage == "" || owned == nil {
		t.Fatal("no observed staging directory")
	}
	final, err := os.Lstat(created.Path)
	if err != nil || !os.SameFile(owned, final) || !os.SameFile(created.owned, final) {
		t.Fatal("publication changed owned directory identity", err)
	}
	if _, err = os.Lstat(stage); !os.IsNotExist(err) {
		t.Fatal("staging remains after publication", err)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 2 {
		t.Fatal(entries, err)
	}
	for _, entry := range entries {
		if !backupID.MatchString(entry.Name()) {
			t.Fatal("nonfinal entry remains", entry.Name())
		}
		if _, err := Inspect(t.Context(), filepath.Join(root, entry.Name())); err != nil {
			t.Fatal("published name was not a complete verified bundle", err)
		}
	}
	if !reflect.DeepEqual(priorBytes, restoreTree(t, prior.Path)) || !reflect.DeepEqual(sourceRows, backupRows(t, f.store.DB())) {
		t.Fatal("publication changed prior bundle or source rows")
	}
}

func TestBundlePublicationCancellationCleansOnlyOwnedStage(t *testing.T) {
	f := newRestoreFixture(t)
	root := filepath.Join(t.TempDir(), "bundles")
	prior, err := Create(t.Context(), f.store, f.dir, root)
	if err != nil {
		t.Fatal(err)
	}
	before := restoreTree(t, prior.Path)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	_, err = createBundleWithClock(ctx, f.store.DB(), f.store.InstanceID(), f.dir, root, func() time.Time { cancel(); return time.Now() }, time.Since)
	backupCode(t, err, "backup_unavailable")
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 1 || entries[0].Name() != prior.ID || !reflect.DeepEqual(before, restoreTree(t, prior.Path)) {
		t.Fatal("cancelled producer left staging or changed prior publication", entries, err)
	}
}

func TestBundlePublicationPreservesExistingFinalCollision(t *testing.T) {
	for _, nonempty := range []bool{false, true} {
		t.Run(map[bool]string{false: "empty", true: "nonempty"}[nonempty], func(t *testing.T) {
			f := newRestoreFixture(t)
			root := filepath.Join(t.TempDir(), "bundles")
			var final string
			var original os.FileInfo
			_, err := createBundleWithClock(t.Context(), f.store.DB(), f.store.InstanceID(), f.dir, root, func() time.Time {
				entries, err := os.ReadDir(root)
				if err != nil || len(entries) != 1 {
					t.Fatal(entries, err)
				}
				id := strings.TrimSuffix(strings.TrimPrefix(entries[0].Name(), "."), ".pending")
				if !backupID.MatchString(id) {
					t.Fatal("invalid staged ID", entries[0].Name())
				}
				final = filepath.Join(root, id)
				if err = os.Mkdir(final, 0700); err != nil {
					t.Fatal(err)
				}
				if nonempty {
					if err = os.WriteFile(filepath.Join(final, "owned-sentinel"), []byte("preserve existing destination"), 0600); err != nil {
						t.Fatal(err)
					}
				}
				original, err = os.Lstat(final)
				if err != nil {
					t.Fatal(err)
				}
				return time.Now()
			}, time.Since)
			backupCode(t, err, "backup_exists")
			current, err := os.Lstat(final)
			if err != nil || !os.SameFile(original, current) {
				t.Fatal("existing final directory replaced", err)
			}
			entries, err := os.ReadDir(root)
			if err != nil || len(entries) != 1 || entries[0].Name() != filepath.Base(final) {
				t.Fatal("collision left owned staging", entries, err)
			}
			files, err := os.ReadDir(final)
			want := 0
			if nonempty {
				want = 1
			}
			if err != nil || len(files) != want {
				t.Fatal("existing directory contents changed", files, err)
			}
			if nonempty {
				body, err := os.ReadFile(filepath.Join(final, "owned-sentinel"))
				if err != nil || string(body) != "preserve existing destination" {
					t.Fatal("existing file changed", err)
				}
			}
		})
	}
}

func TestBundlePublicationManagedRegistrationKeepsDirectoryIdentity(t *testing.T) {
	f, m := newManagedFixture(t, 1)
	result, err := createManaged(t.Context(), f.store, f.dir, m.config.Destination)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(result.Path)
	if err != nil || !os.SameFile(result.owned, info) {
		t.Fatal("managed producer lost original directory identity", err)
	}
	if err = m.registerCreated(t.Context(), result, managedStamp(time.Now())); err != nil {
		t.Fatal("staged publication failed exact ownership registration", err)
	}
	var state, hash string
	if err = f.store.DB().QueryRow(`SELECT state,manifest_sha256 FROM managed_backup_bundles WHERE id=?`, result.ID).Scan(&state, &hash); err != nil || state != "retained" || hash != result.hash {
		t.Fatal(state, hash, err)
	}
	if _, err = Inspect(t.Context(), result.Path); err != nil {
		t.Fatal(err)
	}
}

func TestBundlePublicationCancellationPreservesReplacedStage(t *testing.T) {
	f := newRestoreFixture(t)
	root := filepath.Join(t.TempDir(), "bundles")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var stage, parked string
	var replacement os.FileInfo
	_, err := createBundleWithClock(ctx, f.store.DB(), f.store.InstanceID(), f.dir, root, func() time.Time {
		entries, err := os.ReadDir(root)
		if err != nil || len(entries) != 1 {
			t.Fatal(entries, err)
		}
		stage = filepath.Join(root, entries[0].Name())
		parked = stage + ".parked"
		if err = os.Rename(stage, parked); err != nil {
			t.Fatal(err)
		}
		if err = os.Mkdir(stage, 0700); err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(filepath.Join(stage, "replacement-sentinel"), []byte("preserve replacement"), 0600); err != nil {
			t.Fatal(err)
		}
		replacement, err = os.Lstat(stage)
		if err != nil {
			t.Fatal(err)
		}
		cancel()
		return time.Now()
	}, time.Since)
	backupCode(t, err, "backup_unavailable")
	info, err := os.Lstat(stage)
	if err != nil || !os.SameFile(info, replacement) {
		t.Fatal("cleanup removed another directory at the reserved path", err)
	}
	body, err := os.ReadFile(filepath.Join(stage, "replacement-sentinel"))
	if err != nil || string(body) != "preserve replacement" {
		t.Fatal("replacement contents changed", err)
	}
	if _, err = os.Lstat(parked); err != nil {
		t.Fatal("cleanup searched for and removed displaced reservation", err)
	}
}
