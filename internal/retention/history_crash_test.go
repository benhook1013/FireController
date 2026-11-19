package retention

import (
	"bufio"
	"database/sql/driver"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviews"
	"modernc.org/sqlite"
)

func TestRetentionKillInFlightBatchFencesHistory(t *testing.T) {
	const env = "FC_TEST_RETENTION_CRASH_DIR"
	const marker = "retention-child-inside-second-delete-transaction"
	config := Config{Interval: DefaultInterval, ReviewHistory: DefaultHistory, JobHistory: DefaultHistory, Receipts: DefaultHistory, Diagnostics: DefaultDiagnostics}
	if dir := os.Getenv(env); dir != "" {
		sqlite.MustRegisterScalarFunction("retention_hold_synthetic_delete", 0, func(_ *sqlite.FunctionContext, _ []driver.Value) (driver.Value, error) {
			fmt.Println(marker)
			time.Sleep(30 * time.Second)
			return int64(0), nil
		})
		store, err := core.Open(t.Context(), dir)
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		pruneReviewHistory(t.Context(), &pass{store: store, deleted: map[string]int64{}}, time.Now().Add(-config.ReviewHistory))
		t.Fatal("child did not stop inside the synthetic deletion transaction")
	}
	dir := filepath.Join(t.TempDir(), "private")
	store, err := core.Open(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err = reviews.EnsureSchema(t.Context(), store); err != nil {
		t.Fatal(err)
	}
	s1, s2, s3 := core.NewID("review_snapshot"), core.NewID("review_snapshot"), core.NewID("review_snapshot")
	for _, q := range []string{
		`INSERT INTO projects(id,name,github_repository_id,repository) VALUES('p-ret','Fixture',1,'test/retention')`,
		`WITH RECURSIVE n(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM n WHERE x<600)
INSERT INTO review_pull_versions(project_id,number,version_revision,title,state,head_sha,base_ref,base_sha,ci_state,mergeability,complete,run_count,attempt_count,recent_runs_json,latest_attempts_json)
SELECT 'p-ret',x,1,'old','open',printf('%040d',1),'main',printf('%040d',2),'','',0,0,0,'[]','[]' FROM n`,
		`WITH RECURSIVE n(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM n WHERE x<600)
INSERT INTO review_pull_memberships(project_id,number,start_revision,end_revision,version_revision,position)
SELECT 'p-ret',x,1,2,1,x FROM n`,
	} {
		if strings.Contains(q, "review_pull_versions") {
			old := time.Now().Add(-31 * 24 * time.Hour).UTC().Format(time.RFC3339Nano)
			fresh := time.Now().UTC().Format(time.RFC3339Nano)
			if _, err := store.DB().Exec(`INSERT INTO review_snapshots VALUES('p-ret',?,1,1,'fixture',?, ?,printf('%064d',1)),('p-ret',?,2,1,'fixture',?, ?,printf('%064d',2)),('p-ret',?,3,1,'fixture',?, ?,printf('%064d',3))`, s1, old, old, s2, old, old, s3, fresh, fresh); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := store.DB().Exec(q); err != nil {
			t.Fatal(q, err)
		}
	}
	if _, err := store.DB().Exec(`INSERT INTO review_workspace VALUES('p-ret',3,?)`, s3); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec(`CREATE TRIGGER test_synthetic_hold AFTER DELETE ON review_pull_memberships WHEN OLD.number=550 BEGIN SELECT retention_hold_synthetic_delete(); END`); err != nil {
		t.Fatal(err)
	}
	argv := exec.Command(os.Args[0], "-test.run=^TestRetentionKillInFlightBatchFencesHistory$")
	argv.Env = append(os.Environ(), env+"="+dir)
	output, err := argv.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	argv.Stderr = os.Stderr
	if err = argv.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if argv.ProcessState == nil {
			_ = argv.Process.Kill()
			_ = argv.Wait()
		}
	})
	reached := make(chan bool, 1)
	go func() {
		scan := bufio.NewScanner(output)
		for scan.Scan() {
			if scan.Text() == marker {
				reached <- true
				return
			}
		}
		reached <- false
	}()
	select {
	case yes := <-reached:
		if !yes {
			t.Fatal("child did not enter its second bounded transaction")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("child did not reach the second transaction in time")
	}
	if err = argv.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err = argv.Wait(); err == nil {
		t.Fatal("kill did not interrupt child")
	}
	var retained int
	if err = store.DB().QueryRow(`SELECT count(*) FROM review_pull_memberships WHERE project_id='p-ret'`).Scan(&retained); err != nil || retained != 100 {
		t.Fatal("partial second transaction leaked or first did not commit", retained, err)
	}
	var fence string
	if err = store.DB().QueryRow(`SELECT value FROM metadata WHERE key=?`, core.ReviewHistoryFloorPrefix+"p-ret").Scan(&fence); err != nil || fence != "2" {
		t.Fatal("floor was not durable before deletion", fence, err)
	}
	if err = reviews.ValidateHistoryReferences(t.Context(), store.DB()); err != nil {
		t.Fatal(err)
	}
	token, err := os.ReadFile(filepath.Join(dir, "bootstrap-admin.token"))
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	reviews.Register(mux, store)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/p-ret/reviews/pulls/1?snapshot="+s1, nil)
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
	req.Header.Set("X-FC-Instance", store.InstanceID())
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("below-floor read returned %d, %s", w.Code, w.Body.String())
	}
	if _, err = store.DB().Exec(`DROP TRIGGER test_synthetic_hold`); err != nil {
		t.Fatal(err)
	}
	p := &pass{store: store, deleted: map[string]int64{}}
	if err = pruneReviewHistory(t.Context(), p, time.Now().Add(-config.ReviewHistory)); err != nil {
		t.Fatal(err)
	}
	if p.deleted["review_pull_memberships"] != 100 {
		t.Fatal("did not resume remaining batch", p.deleted)
	}
}
