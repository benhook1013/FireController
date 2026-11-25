package reviewexec

import (
	"context"
	"database/sql"
	"reflect"
	"testing"
)

func TestPlanningSnapshotPinsOrdinaryPullPauseRevisions(t *testing.T) {
	f := schedulingFixture(t)
	read := func() []int64 {
		t.Helper()
		var got []int64
		err := f.store.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
			var err error
			got, err = readAutomaticPullSchedulingRevisions(ctx, tx, attemptProject, []int64{2, 4})
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	before := read()
	schedulingPause(t, f, 2, 0, true)
	paused := read()
	schedulingPause(t, f, 2, 1, false)
	resumed := read()
	for _, c := range []struct{ got, want []int64 }{{before, []int64{0, 0}}, {paused, []int64{1, 0}}, {resumed, []int64{2, 0}}} {
		if !reflect.DeepEqual(c.got, c.want) {
			t.Fatalf("snapshot pin = %v; want %v", c.got, c.want)
		}
	}
}

func TestSchedulingEventPayloadPreservesLegacyAndNativeBasis(t *testing.T) {
	legacy, hosted, cli, ok := readSchedulingEventPayload(sql.NullString{String: `{"automatic_hosted":true}`, Valid: true}, 3)
	if !ok || legacy.Hosted == nil || !*legacy.Hosted || legacy.CLI != nil || hosted != 3 || cli != 3 {
		t.Fatalf("legacy scheduling payload lost its basis: %v %d %d %t", legacy, hosted, cli, ok)
	}
	native, hosted, cli, ok := readSchedulingEventPayload(sql.NullString{String: `{"automatic_hosted":false,"automatic_cli":true,"hosted_basis_revision":2,"cli_basis_revision":3}`, Valid: true}, 3)
	if !ok || native.Hosted == nil || *native.Hosted || native.CLI == nil || !*native.CLI || hosted != 2 || cli != 3 {
		t.Fatalf("native payload lost its channel basis: %v %d %d %t", native, hosted, cli, ok)
	}
}
