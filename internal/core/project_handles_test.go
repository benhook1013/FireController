package core

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestProjectHandleStartupBackfillStableAndIdempotent(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "private")
	s, err := Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.DB().Exec(`DROP TABLE project_handles; DELETE FROM metadata WHERE key='schema:project_handles';
		INSERT INTO projects(id,name,github_repository_id,repository) VALUES
		('project_a','FireMUD',1,'owner/one'),('project_b','FireController',2,'owner/two'),('project_c','FireMUD',3,'owner/three'),('project_d','雪',4,'owner/four');
		CREATE TABLE project_publications(project_id TEXT PRIMARY KEY REFERENCES projects(id),slug TEXT NOT NULL UNIQUE,enabled INTEGER NOT NULL CHECK(enabled IN (0,1)));
		INSERT INTO project_publications(project_id,slug,enabled) VALUES('project_a','firemud',1),('project_d','firecontroller',1)`); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	read := func() [][2]string {
		rows, err := s.DB().Query(`SELECT project_id,handle FROM project_handles ORDER BY project_id`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var values [][2]string
		for rows.Next() {
			var value [2]string
			if err = rows.Scan(&value[0], &value[1]); err != nil {
				t.Fatal(err)
			}
			values = append(values, value)
		}
		return values
	}
	first := read()
	if len(first) != 4 || first[0] != [2]string{"project_a", "firemud"} || first[1] != [2]string{"project_b", suffixedProjectHandle("firecontroller", "project_b", 0)} || first[2] != [2]string{"project_c", suffixedProjectHandle("firemud", "project_c", 0)} || first[3] != [2]string{"project_d", "project-" + stableHandleSuffix("project_d", 0)} {
		t.Fatalf("incorrect deterministic backfill: %#v", first)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if second := read(); !reflect.DeepEqual(first, second) {
		t.Fatalf("restart changed handles: before=%#v after=%#v", first, second)
	}
	var version string
	if err = s.DB().QueryRow(`SELECT value FROM metadata WHERE key='schema:project_handles'`).Scan(&version); err != nil || version != "1" {
		t.Fatalf("wrong handle schema version: %q %v", version, err)
	}
}

func TestProjectHandleAssignmentExplicitAndAutomaticCollisions(t *testing.T) {
	s, err := Open(t.Context(), filepath.Join(t.TempDir(), "private"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err = s.DB().Exec(`INSERT INTO projects(id,name,github_repository_id,repository) VALUES
		('project_one','Shared Name',1,'owner/one'),('project_two','Shared Name',2,'owner/two'),('project_three','Shared Name',3,'owner/three')`); err != nil {
		t.Fatal(err)
	}
	assignName := func(id, name, explicit string) (string, error) {
		tx, err := s.DB().BeginTx(t.Context(), nil)
		if err != nil {
			return "", err
		}
		handle, err := AssignProjectHandle(t.Context(), tx, id, name, explicit)
		if err != nil {
			_ = tx.Rollback()
			return "", err
		}
		return handle, tx.Commit()
	}
	assign := func(id, explicit string) (string, error) { return assignName(id, "Shared Name", explicit) }
	if handle, err := assign("project_one", "chosen"); err != nil || handle != "chosen" {
		t.Fatalf("explicit handle failed: %q %v", handle, err)
	}
	if _, err = assign("project_two", "chosen"); err == nil {
		t.Fatal("explicit collision accepted")
	} else {
		var failure *Error
		if !errors.As(err, &failure) || failure.Code != "project_handle_conflict" || failure.Status != 409 {
			t.Fatalf("incorrect collision refusal: %v", err)
		}
	}
	handle, err := assign("project_two", "")
	if err != nil || handle != "shared-name" {
		t.Fatalf("automatic base handle failed: %q %v", handle, err)
	}
	collision, err := assign("project_three", "")
	if err != nil || collision == handle || !ValidProjectHandle(collision) {
		t.Fatalf("automatic collision suffix failed: %q %v", collision, err)
	}
	if resolved, err := s.ResolveProjectID(t.Context(), collision); err != nil || resolved != "project_three" {
		t.Fatalf("handle did not resolve to canonical ID: %q %v", resolved, err)
	}
	if _, err = s.DB().Exec(`INSERT INTO project_handles(project_id,handle) VALUES('missing','valid')`); err == nil {
		t.Fatal("project handle foreign key accepted missing project")
	}
	if _, err = s.DB().Exec(`INSERT INTO projects(id,name,github_repository_id,repository) VALUES('reserved','Reserved ID',4,'owner/four'),('project_four','Other',5,'owner/five')`); err != nil {
		t.Fatal(err)
	}
	if _, err = assign("project_four", "reserved"); err == nil {
		t.Fatal("handle colliding with another opaque project identity was accepted")
	}
	if _, err = s.DB().Exec(`CREATE TABLE project_publications(project_id TEXT PRIMARY KEY REFERENCES projects(id),slug TEXT NOT NULL UNIQUE,enabled INTEGER NOT NULL CHECK(enabled IN (0,1)));
		INSERT INTO project_publications(project_id,slug,enabled) VALUES('project_one','reserved-pub',1);
		INSERT INTO projects(id,name,github_repository_id,repository) VALUES('project_five','Reserved Pub',6,'owner/six')`); err != nil {
		t.Fatal(err)
	}
	if _, err = assign("project_five", "reserved-pub"); err == nil {
		t.Fatal("explicit handle colliding with another project's publication slug was accepted")
	}
	if automatic, err := assignName("project_five", "Reserved Pub", ""); err != nil || automatic == "reserved-pub" || !strings.HasPrefix(automatic, "reserved-pub-") {
		t.Fatalf("automatic publication collision did not receive a stable suffix: %q %v", automatic, err)
	}
}

func TestProjectHandleSyntax(t *testing.T) {
	for _, value := range []string{"a", "firemud", "project-123", "a-b"} {
		if !ValidProjectHandle(value) {
			t.Fatalf("valid handle refused: %q", value)
		}
	}
	for _, value := range []string{"", "FireMUD", "-start", "end-", "two words", "under_score", "雪"} {
		if ValidProjectHandle(value) {
			t.Fatalf("invalid handle accepted: %q", value)
		}
	}
}
