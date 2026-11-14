package backup

import (
	"encoding/json"
	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviewexec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestIntentLineageExportInspectRestoreAndCorruption(t *testing.T) {
	f := newRestoreFixture(t)
	root, next := core.NewID("review_operation"), core.NewID("review_operation")
	at := time.Now().UTC().Format(time.RFC3339Nano)
	const columns = `id,project_id,actor_id,worker_id,configuration_revision,policy_revision,capability_fingerprint,pull_number,channel,head_sha,state,revision,reason,attempt_id,created_at,updated_at,allow_out_of_order,override_file_limit`
	_, err := f.store.DB().Exec(`INSERT INTO review_operations(`+columns+`) SELECT ?,project_id,actor_id,worker_id,configuration_revision,policy_revision,capability_fingerprint,pull_number,channel,head_sha,'cancelled',2,'source_superseded','',?,?,allow_out_of_order,override_file_limit FROM review_operations LIMIT 1`, root, at, at)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.store.DB().Exec(`INSERT INTO review_operations(`+columns+`,intent_predecessor_id) SELECT ?,project_id,actor_id,worker_id,configuration_revision,policy_revision,capability_fingerprint,pull_number,channel,?,'cancelled',2,'cancelled','',?,?,allow_out_of_order,override_file_limit,? FROM review_operations WHERE id=?`, next, strings.Repeat("c", 40), at, at, root, root)
	if err != nil {
		t.Fatal(err)
	}
	source := reviewexec.SourceObservation{RepositoryID: 991, RepositoryName: "synthetic/restore", PullNumber: 2, HeadSHA: strings.Repeat("c", 40), BaseSHA: strings.Repeat("b", 40), BaseRef: "main", ParentSHA: strings.Repeat("b", 40), ObservedAt: at}
	body, _ := json.Marshal(source)
	_, err = f.store.DB().Exec(`INSERT INTO review_intent_rebindings VALUES(?,?,?,?,?,'',0,0,1,?,?)`, next, root, root, f.project, at, at, string(body))
	if err != nil {
		t.Fatal(err)
	}
	bundle := restoreBundle(t, f)
	if _, err = Inspect(t.Context(), bundle.Path); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(t.TempDir(), "restored-intent")
	if _, err = Restore(t.Context(), bundle.Path, dest); err != nil {
		t.Fatal(err)
	}
	db, err := openSnapshot(t.Context(), filepath.Join(dest, "firecontroller.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var count int
	if err = db.QueryRow(`SELECT count(*) FROM review_intent_rebindings WHERE successor_operation_id=?`, next).Scan(&count); err != nil || count != 1 {
		t.Fatal(count, err)
	}
	for _, mutation := range []string{`DELETE FROM review_intent_rebindings`, `UPDATE review_intent_rebindings SET cli_exhaustions=1`, `UPDATE review_operations SET intent_predecessor_id=NULL`, `DROP INDEX review_intent_root`} {
		bad := restoreModifiedBundle(t, bundle, mutation)
		if _, err = Inspect(t.Context(), bad); err == nil {
			t.Fatal("accepted corrupt intent lineage", mutation)
		}
	}
}
