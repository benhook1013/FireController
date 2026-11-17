package reviewexec

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviewprovider"
)

func checkpointStepClock(e *Engine, at time.Time) *atomic.Int64 {
	samples := &atomic.Int64{}
	e.checkpointNow = func() time.Time {
		return at.Add(-time.Duration(samples.Add(1)-1) * time.Second)
	}
	return samples
}

func checkpointClockHistory(t *testing.T, f *attemptFixture, id string) []string {
	t.Helper()
	rows, err := f.store.DB().QueryContext(t.Context(), `SELECT body_json FROM review_checkpoint_revisions WHERE publication_id=? ORDER BY revision`, id)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var bodies []string
	var previous time.Time
	var created string
	for rows.Next() {
		var body string
		if err := rows.Scan(&body); err != nil {
			t.Fatal(err)
		}
		r, err := decodeCheckpoint(body)
		if err != nil {
			t.Fatal("clock correction produced invalid retained revision", err)
		}
		if created == "" {
			created = r.View.CreatedAt
		}
		at, _ := checkpointTime(r.View.UpdatedAt)
		if r.View.CreatedAt != created || at.Before(previous) || r.View.Revision != int64(len(bodies)+1) {
			t.Fatal("checkpoint history lost immutable origin or coherent chronology", r.View)
		}
		previous = at
		bodies = append(bodies, body)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return bodies
}

func TestCheckpointClockStepLifecycleAndExactReplay(t *testing.T) {
	f, _, evidence := checkpointFixture(t, 0)
	source := encoded(adjudicationRead(t, f, evidence))
	samples := checkpointStepClock(f.e, time.Date(2030, 1, 1, 12, 0, 0, 0, time.UTC))
	r, original := checkpointRequest(t, f, evidence, "clock-lifecycle")
	p := &checkpointFake{}
	worked, err := f.e.checkpointNext(t.Context(), map[string]CheckpointProvider{"test-capability": p})
	if err != nil || !worked {
		t.Fatal("backwards local clock rejected legitimate progression", worked, err)
	}
	v := checkpointRead(t, f, r.ID)
	if v.State != "published" || v.Revision != 4 || v.UpdatedAt != v.CreatedAt || samples.Load() != 4 {
		t.Fatal("one effective timestamp per checkpoint mutation was not retained", v, samples.Load())
	}
	history := checkpointClockHistory(t, f, r.ID)
	if len(history) != 4 {
		t.Fatal("unexpected lifecycle history", len(history))
	}
	var posting checkpointRecord
	if json.Unmarshal([]byte(history[2]), &posting) != nil || posting.Intent == nil || posting.Intent.EarliestPosting != posting.View.UpdatedAt {
		t.Fatal("posting intent did not share its mutation's timestamp")
	}
	// The provider clock is deliberately earlier than local journal time. It is
	// preserved independently rather than clamped or used as elapsed duration.
	p.mu.Lock()
	providerAt := p.comments[0].CreatedAt.Format("2006-01-02T15:04:05.000000000Z")
	p.mu.Unlock()
	if v.PublishedAt != providerAt || v.PublishedAt >= v.CreatedAt {
		t.Fatal("provider time was reclocked", v)
	}
	_, replay := checkpointRequest(t, f, evidence, "clock-lifecycle")
	if !bytes.Equal(original, replay) || samples.Load() != 4 || !reflect.DeepEqual(history, checkpointClockHistory(t, f, r.ID)) || source != encoded(adjudicationRead(t, f, evidence)) {
		t.Fatal("exact replay resampled time or changed original source/history")
	}
}

type checkpointClockProvider struct {
	*checkpointFake
	afterActor, afterPost, afterComments context.CancelFunc
}

func (p *checkpointClockProvider) CurrentActor(ctx context.Context) (reviewprovider.Author, error) {
	a, err := p.checkpointFake.CurrentActor(ctx)
	if p.afterActor != nil {
		p.afterActor()
	}
	return a, err
}
func (p *checkpointClockProvider) PostIssueComment(ctx context.Context, repo string, number int64, body string) (reviewprovider.IssueComment, error) {
	c, err := p.checkpointFake.PostIssueComment(ctx, repo, number, body)
	if p.afterPost != nil {
		p.afterPost()
	}
	return c, err
}
func (p *checkpointClockProvider) CheckpointComments(ctx context.Context, repo string, number int64, actor reviewprovider.Author) ([]reviewprovider.IssueComment, error) {
	comments, err := p.checkpointFake.CheckpointComments(ctx, repo, number, actor)
	if p.afterComments != nil {
		p.afterComments()
	}
	return comments, err
}

func TestCheckpointClockStepStartupAndLostPostRecovery(t *testing.T) {
	for _, interrupted := range []string{"preparing", "posting", "recovering"} {
		t.Run(interrupted, func(t *testing.T) {
			f, _, evidence := checkpointFixture(t, 0)
			checkpointStepClock(f.e, time.Date(2030, 1, 1, 12, 0, 0, 0, time.UTC))
			r, _ := checkpointRequest(t, f, evidence, "clock-interrupted")
			p := &checkpointClockProvider{checkpointFake: &checkpointFake{}}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			switch interrupted {
			case "preparing":
				p.afterActor = cancel
			case "posting":
				p.afterPost = cancel
			case "recovering":
				p.postError = errors.New("synthetic lost response")
			}
			if worked, err := f.e.checkpointNext(ctx, map[string]CheckpointProvider{"test-capability": p}); !worked || err != nil {
				t.Fatal(worked, err)
			}
			var recoveryReceipt []byte
			var recoveryRequest CheckpointPublicationRecovery
			if interrupted == "recovering" {
				v := checkpointRead(t, f, r.ID)
				if v.State != "uncertain" {
					t.Fatal("lost successful POST not retained", v)
				}
				recoveryRequest.ExpectedRevision = &v.Revision
				var err error
				recoveryReceipt, err = f.e.RecoverCheckpointPublication(t.Context(), f.scope(t, attemptProject, f.writer, false), "clock-recovery", r.ID, recoveryRequest)
				if err != nil {
					t.Fatal("recovery request after clock step failed", err)
				}
				p.postError, p.afterComments = nil, cancel
				if worked, err := f.e.checkpointNext(ctx, map[string]CheckpointProvider{"test-capability": p}); !worked || err != nil {
					t.Fatal(worked, err)
				}
			}
			v := checkpointRead(t, f, r.ID)
			if v.State != interrupted {
				t.Fatal("fixture failed to retain interrupted state", v)
			}
			prefix := checkpointClockHistory(t, f, r.ID)
			var intent string
			if interrupted != "preparing" {
				if err := f.store.DB().QueryRow(`SELECT body_json FROM review_checkpoint_intents WHERE publication_id=?`, r.ID).Scan(&intent); err != nil {
					t.Fatal(err)
				}
			}
			// Restart the real store and engine with a still earlier local sample.
			var path string
			if err := f.store.DB().QueryRow(`SELECT file FROM pragma_database_list WHERE name='main'`).Scan(&path); err != nil {
				t.Fatal(err)
			}
			if err := f.store.Close(); err != nil {
				t.Fatal(err)
			}
			var err error
			f.store, err = core.Open(t.Context(), filepath.Dir(path))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = f.store.Close() })
			f.httpFixture.store = f.store
			f.e, err = New(f.store, attemptCapabilities)
			if err != nil {
				t.Fatal(err)
			}
			samples := checkpointStepClock(f.e, time.Date(2029, 1, 1, 0, 0, 0, 0, time.UTC))
			p.afterActor, p.afterPost, p.afterComments = nil, nil, nil
			stop, done := checkpointStart(t, f.e, p)
			v = checkpointTerminal(t, f, r.ID)
			checkpointStop(t, stop, done)
			if v.State != "published" || v.UpdatedAt != v.CreatedAt {
				t.Fatal("startup lost clock-safe progression", v)
			}
			history := checkpointClockHistory(t, f, r.ID)
			if !reflect.DeepEqual(prefix, history[:len(prefix)]) {
				t.Fatal("restart rewrote immutable revisions")
			}
			if intent != "" {
				var after string
				if err := f.store.DB().QueryRow(`SELECT body_json FROM review_checkpoint_intents WHERE publication_id=?`, r.ID).Scan(&after); err != nil || after != intent {
					t.Fatal("recovery reclocked original posting intent", err)
				}
			}
			posts, reads := p.counts()
			wantReads := map[string]int{"preparing": 0, "posting": 1, "recovering": 2}[interrupted]
			if posts != 1 || reads != wantReads {
				t.Fatal("clock correction repeated provider work", posts, reads)
			}
			if recoveryReceipt != nil {
				before := samples.Load()
				replay, err := f.e.RecoverCheckpointPublication(t.Context(), f.scope(t, attemptProject, f.writer, false), "clock-recovery", r.ID, recoveryRequest)
				if err != nil || !bytes.Equal(recoveryReceipt, replay) || before != samples.Load() || !reflect.DeepEqual(history, checkpointClockHistory(t, f, r.ID)) {
					t.Fatal("historical recovery replay resampled time", err)
				}
			}
		})
	}
}

func TestCheckpointClockStepDoesNotRepairMalformedRetainedTimes(t *testing.T) {
	for _, corrupt := range []string{"not-a-time", "2030-01-01T12:00:00Z", "2029-01-01T00:00:00.000000000Z"} {
		t.Run(corrupt, func(t *testing.T) {
			f, _, evidence := checkpointFixture(t, 0)
			samples := checkpointStepClock(f.e, time.Date(2030, 1, 1, 12, 0, 0, 0, time.UTC))
			r, _ := checkpointRequest(t, f, evidence, "clock-corrupt")
			var body string
			if err := f.store.DB().QueryRow(`SELECT body_json FROM review_checkpoint_publications WHERE id=?`, r.ID).Scan(&body); err != nil {
				t.Fatal(err)
			}
			var record checkpointRecord
			if err := json.Unmarshal([]byte(body), &record); err != nil {
				t.Fatal(err)
			}
			record.View.UpdatedAt = corrupt
			body = encoded(record)
			_, err := decodeCheckpoint(body)
			attemptCode(t, err, "storage_unavailable")
			admissionSQL(t, f, `UPDATE review_checkpoint_publications SET body_json=? WHERE id=?`, body, r.ID)
			_, err = f.e.RecoverCheckpointPublication(t.Context(), f.scope(t, attemptProject, f.writer, false), "clock-corrupt-recovery", r.ID, CheckpointPublicationRecovery{ExpectedRevision: &r.Revision})
			attemptCode(t, err, "storage_unavailable")
			if samples.Load() != 1 {
				t.Fatal("malformed history was reclocked")
			}
			var after string
			if err := f.store.DB().QueryRow(`SELECT body_json FROM review_checkpoint_publications WHERE id=?`, r.ID).Scan(&after); err != nil || after != body {
				t.Fatal("malformed retained row was rewritten", err)
			}
		})
	}
}
