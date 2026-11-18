package reviewexec

import (
	"encoding/json"
	"testing"
)

func TestOld649RuntimeSortingConfigurationCompatibility(t *testing.T) {
	for _, mode := range []string{"", "auto", "manual"} {
		t.Run("mode-"+mode, func(t *testing.T) {
			f := newAttemptFixture(t)
			f.configure(t, attemptProject, "test-capability", 0, false)
			// Synthetic retained JSON only. Current candidate Configure/backup provenance
			// is qualified separately; no production database is read or modified here.
			if mode != "" {
				var raw string
				if err := f.store.DB().QueryRow(`SELECT body_json FROM review_execution_config WHERE project_id=? AND revision=1`, attemptProject).Scan(&raw); err != nil {
					t.Fatal(err)
				}
				var fields map[string]any
				if err := json.Unmarshal([]byte(raw), &fields); err != nil {
					t.Fatal(err)
				}
				fields["queue_sort_mode"] = mode
				body, err := json.Marshal(fields)
				if err != nil {
					t.Fatal(err)
				}
				if _, err = f.store.DB().Exec(`UPDATE review_execution_config SET body_json=? WHERE project_id=? AND revision=1`, string(body), attemptProject); err != nil {
					t.Fatal(err)
				}
			}
			c, err := f.e.ReadExecutionConfiguration(t.Context(), f.scope(t, attemptProject, f.writer, false))
			if mode == "" {
				if err != nil || c.Revision != 1 {
					t.Fatal(c, err)
				}
				t.Log("legacy configuration accepted")
			} else {
				if err == nil {
					t.Fatal("unexpected old runtime acceptance", mode, c)
				}
				t.Logf("explicit %s refused by actual old ReadExecutionConfiguration: %v", mode, err)
			}
		})
	}
}
