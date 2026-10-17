package backup

import (
	"context"
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviewexec"
	"github.com/benhook1013/FireController/internal/reviewprovider"
)

type selectionInventoryReader struct {
	state   string
	entered chan struct{}
	once    sync.Once
	calls   atomic.Int64
}

func (r *selectionInventoryReader) Repository(ctx context.Context, id int64) (reviewprovider.Repository, error) {
	r.calls.Add(1)
	if r.state == "selecting" {
		r.once.Do(func() { close(r.entered) })
		<-ctx.Done()
		return reviewprovider.Repository{}, ctx.Err()
	}
	return reviewprovider.Repository{ID: id, FullName: "synthetic/restore", DefaultBranch: "main"}, nil
}
func (r *selectionInventoryReader) Pull(_ context.Context, _ string, n int64) (reviewprovider.Pull, error) {
	r.calls.Add(1)
	state := "open"
	merged := r.state == "empty"
	if merged {
		state = "closed"
	}
	return reviewprovider.Pull{Number: n, RepositoryID: 991, State: state, Merged: merged, HeadSHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("b", 40), BaseRef: "main", ChangedFiles: 1}, nil
}
func selectionInventoryEngine(t *testing.T, s *core.Store) *reviewexec.Engine {
	t.Helper()
	e, err := reviewexec.New(s, []reviewexec.Capability{{ID: "restore-cap", Provider: "coderabbit", Account: "fixture-account", Channels: []string{"hosted"}, Fingerprint: strings.Repeat("f", 64)}, {ID: "selection-cli", Provider: "coderabbit", Account: "selection-account", Channels: []string{"cli"}}})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// The real source worker chooses and atomically links an ordinary operation;
// no SQL fixture manufactures a selected child or captured completion credit.
func selectionInventoryFixture(t *testing.T, state string) (*restoreFixture, string) {
	t.Helper()
	f := newRestoreFixture(t)
	e := selectionInventoryEngine(t, f.store)
	scope := routePolicyInventoryScope(t, f)
	one := int64(1)
	if _, err := e.Configure(t.Context(), scope, "selection-inventory-config", reviewexec.Configuration{ExpectedRevision: &one, CapabilityID: "selection-cli", Mode: "independent", Queue: []int64{4}, Channels: []string{"cli"}}); err != nil {
		t.Fatal(err)
	}
	input := reviewexec.RunRequest{Channel: "cli"}
	if state == "held" {
		other := int64(99)
		input.ExpectedPullNumber = &other
	}
	raw, err := e.EnqueueRun(t.Context(), scope, "selection-inventory-run", input)
	if err != nil {
		t.Fatal(err)
	}
	var receipt reviewexec.Receipt
	if json.Unmarshal(raw, &receipt) != nil {
		t.Fatal("invalid selection receipt")
	}
	if state != "queued" {
		reader := &selectionInventoryReader{state: state, entered: make(chan struct{})}
		ctx, cancel := context.WithCancel(t.Context())
		done, err := e.StartReviewRuns(ctx, map[string]reviewexec.TargetReader{"selection-cli": reader})
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		t.Cleanup(cancel)
		if state == "selecting" {
			select {
			case <-reader.entered:
			case <-time.After(5 * time.Second):
				cancel()
				t.Fatal("source reader not reached")
			}
		} else {
			bounded, stop := context.WithTimeout(t.Context(), 5*time.Second)
			r, complete, err := e.WaitRun(bounded, scope, receipt.ID, 5)
			stop()
			if err != nil || !complete || r.State != state {
				cancel()
				t.Fatal("actual selection state mismatch", r.State, err)
			}
		}
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("owned selection worker did not join")
		}
	}
	r, err := e.ReadRun(t.Context(), scope, receipt.ID)
	if err != nil || r.State != state {
		t.Fatal("retained selection mismatch", err)
	}
	if (state == "selected") != (r.OperationID != "") {
		t.Fatal("selection child shape incorrect")
	}
	if err = reviewexec.ValidateSelectionSnapshot(t.Context(), f.store.DB()); err != nil {
		t.Fatal("genuine selection failed validation", err)
	}
	return f, receipt.ID
}
func TestSelectionInventoryRealRunRoundTrip(t *testing.T) {
	for _, state := range []string{"queued", "selecting", "selected", "held", "empty"} {
		t.Run(state, func(t *testing.T) {
			f, _ := selectionInventoryFixture(t, state)
			before := backupRows(t, f.store.DB())
			bundle := restoreBundle(t, f)
			immutable := restoreTree(t, bundle.Path)
			if _, err := Inspect(t.Context(), bundle.Path); err != nil {
				t.Fatal(err)
			}
			destination := filepath.Join(t.TempDir(), "inactive-selection")
			if _, err := Restore(t.Context(), bundle.Path, destination); err != nil {
				t.Fatal(err)
			}
			db, err := openSnapshot(t.Context(), filepath.Join(destination, "firecontroller.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if err = reviewexec.ValidateSelectionSnapshot(t.Context(), db); err != nil {
				t.Fatal(err)
			}
			after := backupRows(t, db)
			for table, hash := range before {
				if table != "metadata" && table != "credentials" && after[table] != hash {
					t.Fatalf("restore changed retained table %s", table)
				}
			}
			if !reflect.DeepEqual(before, backupRows(t, f.store.DB())) || !reflect.DeepEqual(immutable, restoreTree(t, bundle.Path)) {
				t.Fatal("backup/inspect/restore mutated source or bundle")
			}
		})
	}
}
func TestSelectionInventoryResealedCorruptionRefused(t *testing.T) {
	for _, tc := range []struct{ name, query string }{
		{"empty-key", `UPDATE review_selections SET id=''`},
		{"blob-key", `UPDATE review_selections SET id=CAST(id AS BLOB)`},
		{"unknown-actor", `UPDATE review_selections SET actor_id='unretained-actor'`},
		{"wrong-worker", `UPDATE review_selections SET worker_id='unretained-worker'`},
		{"wrong-channel", `UPDATE review_selections SET channel='hosted'`},
		{"wrong-expectation", `UPDATE review_selections SET expected_pull_number=3`},
		{"false-policy", `UPDATE review_selections SET capability_fingerprint=replace(capability_fingerprint,'a','0')||'x'`},
		{"invalid-time", `UPDATE review_selections SET updated_at='unknown'`},
		{"false-child-actor", `UPDATE review_operations SET actor_id='another-principal' WHERE id=(SELECT operation_id FROM review_selections)`},
		{"blob-child-binding", `UPDATE review_operations SET capability_fingerprint=CAST(capability_fingerprint AS BLOB) WHERE id=(SELECT operation_id FROM review_selections)`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, _ := selectionInventoryFixture(t, "selected")
			before := backupRows(t, f.store.DB())
			bundle := restoreBundle(t, f)
			mutateBundleSQL(t, bundle.Path, tc.query)
			assertCurrentSummaryInventoryRefused(t, bundle, true)
			if !reflect.DeepEqual(before, backupRows(t, f.store.DB())) {
				t.Fatal("corrupt bundle changed source")
			}
		})
	}
}
func TestSelectionInventorySchemaAndPredecessor(t *testing.T) {
	for _, query := range []string{
		`DROP INDEX review_selections_pending_channel`,
		`DROP INDEX review_selections_pending; CREATE INDEX review_selections_pending ON review_selections(id)`,
		`CREATE INDEX invented_selection_credit ON review_selections(operation_id)`,
		`ALTER TABLE review_selections ADD COLUMN native_credit INTEGER`,
	} {
		t.Run("corrupt-schema", func(t *testing.T) {
			f := newRestoreFixture(t)
			bundle := restoreBundle(t, f)
			mutateBundleSQL(t, bundle.Path, query)
			assertCurrentSummaryInventoryRefused(t, bundle, false)
		})
	}
	t.Run("nullable-id", func(t *testing.T) {
		f := newRestoreFixture(t)
		bundle := restoreBundle(t, f)
		var statements []string
		for _, s := range reviewexec.SelectionSchemaSQL() {
			statements = append(statements, strings.Replace(s, "id TEXT NOT NULL PRIMARY KEY", "id TEXT PRIMARY KEY", 1))
		}
		mutateBundleSQL(t, bundle.Path, "DROP TABLE review_selections;"+strings.Join(statements, ";"))
		assertCurrentSummaryInventoryRefused(t, bundle, false)
	})
	t.Run("absent-optional-domain", func(t *testing.T) {
		f := newRestoreFixture(t)
		var count int
		if err := f.store.DB().QueryRow(`SELECT count(*) FROM review_selections`).Scan(&count); err != nil || count != 0 {
			t.Fatal("predecessor domain not empty", err)
		}
		if _, err := f.store.DB().Exec(`DROP TABLE review_selections;DELETE FROM metadata WHERE key='schema:review_selection'`); err != nil {
			t.Fatal(err)
		}
		bundle := restoreBundle(t, f)
		if _, err := Inspect(t.Context(), bundle.Path); err != nil {
			t.Fatal(err)
		}
	})
}
