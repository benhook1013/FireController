package reviewexec

import "testing"

func TestSelectionNativeBlobKeyDoesNotStopRecovery(t *testing.T) {
	for _, state := range []string{"queued", "selecting"} {
		t.Run(state, func(t *testing.T) {
			f := selectionFixture(t)
			r, _ := selectionEnqueue(t, f, "blob-selection", "hosted", nil)
			admissionSQL(t, f, `UPDATE review_selections SET id=CAST(id AS BLOB),state=? WHERE id=?`, state, r.ID)
			if state == "selecting" {
				if err := f.e.RecoverReviewRuns(t.Context()); err != nil {
					t.Fatal(err)
				}
			} else {
				if _, err := f.e.runNext(t.Context(), nil); err != nil {
					t.Fatal(err)
				}
			}
			var got, reason string
			if err := f.store.DB().QueryRow(`SELECT state,reason FROM review_selections WHERE id=CAST(? AS BLOB)`, r.ID).Scan(&got, &reason); err != nil {
				t.Fatal(err)
			}
			if got != "held" || reason != "local_read_unavailable" {
				t.Fatal(got, reason)
			}
		})
	}
}
