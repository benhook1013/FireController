package reviewruntime

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/reviewprovider"
)

func TestHostedSchedulerSuccessfulPendingKeepsShortCadenceThenResets(t *testing.T) {
	for _, kind := range []string{"publication_not_proven", "missing_publication_batches"} {
		t.Run(kind, func(t *testing.T) {
			f, a, p := schedulerUncertainFixture(t)
			if kind == "publication_not_proven" {
				f.provider.partial = true
			} else {
				p.alterSnapshot = func(snapshot *reviewprovider.HostedPublicationSnapshot) {
					snapshot.Reviews[0].Body = strings.Replace(snapshot.Reviews[0].Body, "batch=1/1", "batch=1/2", 1)
				}
			}
			before, err := f.engine.ReadHostedObservations(t.Context(), f.project, a.ID)
			if err != nil {
				t.Fatal(err)
			}
			gate := newSchedulerGate(f.hosted)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done, err := f.hosted.StartRecovery(ctx)
			if err != nil {
				t.Fatal(err)
			}
			for index, seconds := range []int{30, 30, 30, 30, 30, 30} {
				if index > 0 {
					gate.resume(t)
				}
				gate.interval(t, time.Duration(seconds)*time.Second)
				references, err := f.engine.ReadHostedObservations(t.Context(), f.project, a.ID)
				if err != nil || len(references) != len(before)+index+1 || hostedDriverJournal(t, f, a.ID) != "active" || f.provider.observations != index+1 || cliCount(t, f.store, `SELECT count(*) FROM review_execution_evidence`) != 0 || cliCount(t, f.store, `SELECT count(*) FROM review_execution_reservations`) != 1 {
					t.Fatal("successful pending pass lost raw evidence/ownership", index, len(references), err)
				}
				ref := references[len(references)-1].Input
				if _, err := f.hostedCaptures.Read(t.Context(), a.ID, CaptureReference{ID: ref.CaptureID, SHA256: ref.CaptureSHA256, Bytes: ref.CaptureBytes}); err != nil {
					t.Fatal("pending raw observation unreadable", err)
				}
				if f.provider.queries != 1 || f.provider.reviews != 1 || p.currentActors.Load() != 0 {
					t.Fatal("pending retry issued another provider action")
				}
			}
			f.provider.partial = false
			p.alterSnapshot = nil
			gate.resume(t)
			gate.interval(t, 15*time.Second)
			if hostedDriverJournal(t, f, a.ID) != "completed" || f.provider.observations != 7 || cliCount(t, f.store, `SELECT count(*) FROM review_execution_evidence`) != 1 || cliCount(t, f.store, `SELECT count(*) FROM review_execution_reservations`) != 0 {
				t.Fatal("completion did not reset cadence and settle once")
			}
			observations, repositories, events := f.provider.observations, p.repositories.Load(), cliCount(t, f.store, `SELECT count(*) FROM events`)
			gate.resume(t)
			gate.interval(t, 15*time.Second)
			if f.provider.observations != observations || p.repositories.Load() != repositories || cliCount(t, f.store, `SELECT count(*) FROM events`) != events || f.provider.queries != 1 || f.provider.reviews != 1 {
				t.Fatal("empty reset tick performed provider/domain work")
			}
			cancel()
			schedulerDone(t, done, false)
		})
	}
}
