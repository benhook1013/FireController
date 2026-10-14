package profile_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/benhook1013/FireController/internal/profile"
)

func writeProfile(t *testing.T, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "profiles.json")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadUsesStrictDecodeForAmbiguitiesAndNullShapes(t *testing.T) {
	valid := `{"version":1,"profiles":{"chosen":{"server_url":"http://localhost:8080","instance_id":"instance-one","project_id":"project-one","worker_id":"worker-one","credential_file":"private/missing.token"}}}`
	for _, tc := range []struct {
		name string
		data []byte
		code string
	}{
		{name: "duplicate key", data: []byte(`{"version":1,"version":1,"profiles":{}}`), code: "profiles_invalid_json"},
		{name: "case alias", data: []byte(`{"version":1,"profiles":{"chosen":{"server_url":"http://localhost","instance_id":"one","project_id":"project","worker_id":"worker","credential_file":"token","WORKER_ID":"other"}}}`), code: "profiles_invalid_json"},
		{name: "unknown field", data: []byte(`{"version":1,"profiles":{"chosen":{"server_url":"http://localhost","instance_id":"one","project_id":"project","worker_id":"worker","credential_file":"token","extra":true}}}`), code: "profiles_invalid_json"},
		{name: "trailing object", data: []byte(valid + `{}`), code: "profiles_invalid_json"},
		{name: "null root", data: []byte(`null`), code: "profiles_invalid_json"},
		{name: "null entry", data: []byte(`{"version":1,"profiles":{"chosen":null}}`), code: "profiles_invalid_json"},
		{name: "null profiles map", data: []byte(`{"version":1,"profiles":null}`), code: "profiles_invalid"},
		{name: "unpaired surrogate", data: []byte(`{"version":1,"profiles":{"chosen":{"server_url":"http://localhost","instance_id":"\ud800","project_id":"project","worker_id":"worker","credential_file":"token"}}}`), code: "profiles_invalid_encoding"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := profile.Load(writeProfile(t, tc.data), "chosen")
			if err == nil || err.Code != tc.code || err.Message == "" || err.Action == "" {
				t.Fatalf("Load error=%v want code %q", err, tc.code)
			}
		})
	}
}

func TestLoadDoesNotReadOrCreateCredentialReference(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "profiles.json")
	credentialPath := filepath.Join(directory, "private", "missing.token")
	data := []byte(`{"version":1,"profiles":{"chosen":{"server_url":"http://localhost:8080","instance_id":"instance-one","project_id":"project-one","worker_id":"worker-one","credential_file":"private/missing.token"}}}`)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	identity, err := profile.Load(path, "chosen")
	if err != nil {
		t.Fatal(err)
	}
	if identity.Profile != "chosen" || identity.ProjectID != "project-one" {
		t.Fatalf("unexpected local identity: %+v", identity)
	}
	if _, err := os.Stat(credentialPath); !os.IsNotExist(err) {
		t.Fatalf("Load touched credential path %q: err=%v", credentialPath, err)
	}
	if _, err := os.Stat(filepath.Dir(credentialPath)); !os.IsNotExist(err) {
		t.Fatalf("Load created credential directory: err=%v", err)
	}
}
