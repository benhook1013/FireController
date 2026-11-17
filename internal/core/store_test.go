package core

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

func TestDatabaseFileURIPreservesPathWithoutAuthority(t *testing.T) {
	for _, test := range []struct {
		path string
		want string
	}{
		{"/private/controller/firecontroller.sqlite", "file:///private/controller/firecontroller.sqlite"},
		{"C:/Users/operator/controller/firecontroller.sqlite", "file:///C:/Users/operator/controller/firecontroller.sqlite"},
		{"D:/private files/controller#1%/firecontroller.sqlite", "file:///D:/private%20files/controller%231%25/firecontroller.sqlite"},
		{"/private/controller?scope=other/firecontroller.sqlite", "file:///private/controller%3Fscope=other/firecontroller.sqlite"},
	} {
		t.Run(test.path, func(t *testing.T) {
			uri := databaseURL(test.path)
			if uri.String() != test.want || uri.Host != "" || uri.RawQuery != "" || uri.Fragment != "" {
				t.Fatalf("database path changed URI meaning: %s", uri.String())
			}
		})
	}
}

func TestDomainSchemaVersionsMigrateOnceAndRefuseDowngrades(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "private")
	s, err := Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	first := []string{"CREATE TABLE schema_probe(id INTEGER PRIMARY KEY)", "INSERT INTO schema_probe VALUES(1)"}
	second := []string{"ALTER TABLE schema_probe ADD COLUMN label TEXT", "UPDATE schema_probe SET label='retained'"}
	if err := s.EnsureSchema(ctx, "probe", first); err != nil {
		t.Fatal(err)
	}
	if err := s.EnsureSchema(ctx, "probe", first); err != nil {
		t.Fatalf("repeated startup reapplied migration: %v", err)
	}
	if err := s.EnsureSchema(ctx, "probe", first, second); err != nil {
		t.Fatal(err)
	}
	// A newer version is refused before even the supplied destructive DDL runs.
	err = s.EnsureSchema(ctx, "probe", []string{"DROP TABLE schema_probe"})
	var failure *Error
	if !errors.As(err, &failure) || failure.Code != "schema_unsupported" {
		t.Fatalf("newer domain schema accepted: %v", err)
	}
	var value, version string
	if err := s.DB().QueryRow("SELECT label FROM schema_probe WHERE id=1").Scan(&value); err != nil || value != "retained" {
		t.Fatalf("downgrade changed data: %v %s", err, value)
	}
	if err := s.DB().QueryRow("SELECT value FROM metadata WHERE key='schema:probe'").Scan(&version); err != nil || version != "2" {
		t.Fatalf("wrong version: %v %s", err, version)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.EnsureSchema(ctx, "probe", first, second); err != nil {
		t.Fatalf("restart migration failed: %v", err)
	}
}

func TestDomainSchemaFailureAndInvalidMetadataDoNotAdvance(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "private"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	first := []string{"CREATE TABLE domain_probe(id INTEGER)"}
	if err := s.EnsureSchema(ctx, "probe", first); err != nil {
		t.Fatal(err)
	}
	failed := []string{"ALTER TABLE domain_probe ADD COLUMN leaked TEXT", "INVALID DDL"}
	if err := s.EnsureSchema(ctx, "probe", first, failed); err == nil {
		t.Fatal("invalid migration succeeded")
	}
	var version string
	if err := s.DB().QueryRow("SELECT value FROM metadata WHERE key='schema:probe'").Scan(&version); err != nil || version != "1" {
		t.Fatalf("failed migration advanced: %v %s", err, version)
	}
	var columns int
	if err := s.DB().QueryRow("SELECT count(*) FROM pragma_table_info('domain_probe')").Scan(&columns); err != nil || columns != 1 {
		t.Fatal("failed migration leaked column")
	}
	if err := s.EnsureSchema(ctx, "probe", first, []string{"ALTER TABLE domain_probe ADD COLUMN retained TEXT"}); err != nil {
		t.Fatalf("safe retry failed: %v", err)
	}
	for _, raw := range []string{"0", "-1", "garbage", "02", "999999999999999999999999999"} {
		if _, err := s.DB().Exec("UPDATE metadata SET value=? WHERE key='schema:probe'", raw); err != nil {
			t.Fatal(err)
		}
		err := s.EnsureSchema(ctx, "probe", []string{"DROP TABLE domain_probe"})
		var failure *Error
		if !errors.As(err, &failure) || failure.Code != "schema_invalid" {
			t.Fatalf("malformed version accepted: %v", err)
		}
	}
}
