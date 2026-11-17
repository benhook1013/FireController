package offsite

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/benhook1013/FireController/internal/backup"
)

func TestOffsiteEqualSequenceClockRollbackPublishesAndPrunes(t *testing.T) {
	for _, restart := range []bool{false, true} {
		name := "direct"
		if restart {
			name = "pending_restart_without_source"
		}
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			first := f.bundle(t)
			// Only synthetic wall-clock metadata changes. The canonical bundle,
			// source database and event watermark still pass native inspection.
			manifestPath := filepath.Join(first.Path, "manifest.json")
			body, err := os.ReadFile(manifestPath)
			if err != nil {
				t.Fatal(err)
			}
			var manifest backup.Manifest
			if err := json.Unmarshal(body, &manifest); err != nil {
				t.Fatal(err)
			}
			manifest.SnapshotFinishedAt = "2099-01-01T00:00:00Z"
			body, err = json.Marshal(manifest)
			if err != nil {
				t.Fatal(err)
			}
			body = append(body, '\n')
			hash := sha256.Sum256(body)
			if err := os.WriteFile(manifestPath, body, 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(first.Path, "manifest.sha256"), []byte(hex.EncodeToString(hash[:])+"\n"), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := backup.Inspect(t.Context(), first.Path); err != nil {
				t.Fatal(err)
			}
			remote := memory()
			if _, err := push(t.Context(), f.c, remote); err != nil {
				t.Fatal(err)
			}
			old := *state(t, f.c).LastSuccess
			// No write between snapshots: equal event sequences, earlier time.
			next, err := backup.Create(t.Context(), f.store, f.dir, f.c.SourceRoot)
			if err != nil || next.EventSequence != old.EventSequence || next.SnapshotFinishedAt >= old.SnapshotFinishedAt {
				t.Fatal("fixture did not retain equal history across clock rollback", next, err)
			}
			if err := os.RemoveAll(first.Path); err != nil {
				t.Fatal(err)
			}
			var pending *Receipt
			if restart {
				remote.uncertainReceipt = true
				_, err := push(t.Context(), f.c, remote)
				wantCode(t, err, "offsite_transport_failed")
				st := state(t, f.c)
				if st.Pending == nil || st.Pending.ID != next.ID || *st.LastSuccess != old {
					t.Fatal("lost exact pending recovery binding", st)
				}
				pending = st.Pending
				if err := os.RemoveAll(next.Path); err != nil {
					t.Fatal(err)
				}
			}
			// Retain both copies after publication, then retry pruning in a fresh
			// session. The older wall-clock copy must never replace the keeper.
			remote.failDelete = true
			_, err = push(t.Context(), f.c, remote)
			wantCode(t, err, "offsite_transport_failed")
			st := state(t, f.c)
			if st.Pending != nil || st.LastSuccess.ID != next.ID || len(st.Owned) != 2 || st.Owned[0] != *st.LastSuccess || pending != nil && *st.LastSuccess != *pending || st.RetentionReason != "offsite_retention_failed" {
				t.Fatal("rollback prevented publication or lost its exact keeper", st)
			}
			s, err := openSession(f.c, remote)
			if err != nil {
				t.Fatal(err)
			}
			defer s.close()
			published, err := s.receiptAt(t.Context(), f.c.Remote+"/"+f.c.InstanceID+"/latest.json")
			if err != nil || published != *st.LastSuccess {
				t.Fatal("latest did not bind exact successful receipt", published, err)
			}
			remote.failDelete = false
			if err := s.prune(t.Context(), false); err != nil {
				t.Fatal("restart cleanup rejected rollback keeper", err)
			}
			if err := s.save(); err != nil {
				t.Fatal(err)
			}
			after := state(t, f.c)
			if !reflect.DeepEqual(after.Owned, []Receipt{published}) || *after.LastSuccess != published || after.RetentionReason != "" || remote.deletes != 2 || len(remote.files) != 3 {
				t.Fatal("pruning did not preserve exact newly published copy", after, remote.deletes)
			}
			for name, count := range remote.puts {
				if name == s.object(published, "archive.tar.gz") || name == s.object(published, "receipt.json") {
					if count != 1 {
						t.Fatal("pending restart duplicated immutable objects", name, count)
					}
				}
			}
		})
	}
}

func TestOffsiteClockRollbackStillRefusesLowerHistoryAndConflictingKeeper(t *testing.T) {
	f := newFixture(t)
	older := f.bundle(t)
	newer := f.bundle(t)
	remote := memory()
	if _, err := push(t.Context(), f.c, remote); err != nil {
		t.Fatal(err)
	}
	before := state(t, f.c)
	if err := os.RemoveAll(newer.Path); err != nil {
		t.Fatal(err)
	}
	_, err := push(t.Context(), f.c, remote)
	wantCode(t, err, "offsite_source_rollback")
	if after := state(t, f.c); !reflect.DeepEqual(after.Owned, before.Owned) || !reflect.DeepEqual(after.LastSuccess, before.LastSuccess) || after.Pending != nil || remote.deletes != 0 {
		t.Fatal("lower source changed retained history", after)
	}
	s, err := openSession(f.c, remote)
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	lower := *s.state.LastSuccess
	lower.EventSequence = older.EventSequence
	puts := len(remote.puts)
	wantCode(t, s.publishLatest(t.Context(), lower), "offsite_source_rollback")
	if len(remote.puts) != puts {
		t.Fatal("lower history changed latest pointer")
	}
	keeper := *s.state.LastSuccess
	for _, kind := range []string{"higher_owned_sequence", "same_id_different_receipt"} {
		t.Run(kind, func(t *testing.T) {
			other := keeper
			if kind == "higher_owned_sequence" {
				other.ID = older.ID
				other.EventSequence++
				s.state.Owned = []Receipt{keeper, other}
			} else {
				// Matching only the ID cannot establish ownership of the keeper.
				other.ArchiveBytes++
				old := lower
				old.ID = older.ID
				s.state.Owned = []Receipt{other, old}
			}
			wantCode(t, s.prune(t.Context(), false), "offsite_retention_conflict")
			if remote.deletes != 0 || len(remote.puts) != puts {
				t.Fatal("conflicting retention changed remote objects")
			}
		})
	}
}
