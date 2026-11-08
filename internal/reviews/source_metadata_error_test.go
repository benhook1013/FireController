package reviews

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"testing"

	"github.com/benhook1013/FireController/internal/core"
)

func TestSourceMetadataRetainedTypesFailClosedWithoutPanic(t *testing.T) {
	for _, field := range []string{"metadata_json", "state"} {
		for _, stage := range []string{"pin", "publish"} {
			t.Run(field+"/"+stage, func(t *testing.T) {
				f := newAPIFixture(t)
				p := nativePull(1)
				p.State = "open"
				f.observe(t, reviewRequest(0, []PullInput{p}), "retained-types")
				pins := sourcePins(t, f, 1)
				value := []byte("{}")
				if field == "state" {
					value = []byte("open")
				}
				if _, err := f.store.DB().Exec(`UPDATE review_pull_versions SET `+field+`=? WHERE project_id=? AND number=1`, value, f.project); err != nil {
					t.Fatal(err)
				}
				// SQLite TEXT affinity accepts BLOBs; scanning into any retains []byte.
				var kind string
				if err := f.store.DB().QueryRow(`SELECT typeof(`+field+`) FROM review_pull_versions WHERE project_id=? AND number=1`, f.project).Scan(&kind); err != nil || kind != "blob" {
					t.Fatal(kind, err)
				}
				before := historyTableCounts(t, f)
				err := f.store.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
					if stage == "pin" {
						_, err := PinSourceMetadata(ctx, tx, f.project, []int64{1})
						return err
					}
					_, err := PublishSourceMetadata(ctx, tx, core.Scope{ProjectID: f.project, PrincipalID: "test-source"}, 701, pins, []PullInput{sourcePull(1, "must not publish")})
					return err
				})
				var typed *SourceMetadataError
				var failure *core.Error
				if !errors.As(err, &typed) || typed.ProjectID != f.project || typed.PullNumber != 1 || !errors.As(err, &failure) || failure.Code != "storage_unavailable" {
					t.Fatal("missing scoped fail-closed error", err)
				}
				if !reflect.DeepEqual(before, historyTableCounts(t, f)) {
					t.Fatal("malformed display facts published changes")
				}
			})
		}
	}
}
