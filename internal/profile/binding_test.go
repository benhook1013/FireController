package profile_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/benhook1013/FireController/internal/profile"
)

func TestBindingResolvesReferenceWithoutCredentialIO(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "profiles.json")
	data := []byte(`{"version":1,"profiles":{"chosen":{"server_url":"http://localhost:8080/prefix","instance_id":"instance-one","project_id":"project-one","worker_id":"worker-one","credential_file":"private/missing.token"}}}`)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	local, localErr := profile.Load(path, "chosen")
	binding, bindingErr := profile.LoadBinding(path, "chosen")
	if localErr != nil || bindingErr != nil || binding.Context != local || binding.CredentialFile != filepath.Join(dir, "private", "missing.token") {
		t.Fatalf("binding validation/resolution differs: %v %v", localErr, bindingErr)
	}
	encoded, err := json.Marshal(binding)
	if err != nil {
		t.Fatal(err)
	}
	var output map[string]any
	if err = json.Unmarshal(encoded, &output); err != nil || len(output) != 1 || output["context"] == nil {
		t.Fatal("binding exposes credential reference")
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	saved, err := os.ReadFile(path)
	if err != nil || string(saved) != string(data) || !before.ModTime().Equal(after.ModTime()) {
		t.Fatal("profile was modified")
	}
	if _, err = os.Stat(filepath.Join(dir, "private")); !os.IsNotExist(err) {
		t.Fatal("credential resolution performed filesystem mutation")
	}
	// An absolute reference stays absolute and is still not opened.
	absolute := filepath.Join(dir, "absolute-missing.token")
	var config map[string]any
	if err = json.Unmarshal(data, &config); err != nil {
		t.Fatal(err)
	}
	config["profiles"].(map[string]any)["chosen"].(map[string]any)["credential_file"] = absolute
	data, err = json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	binding, bindingErr = profile.LoadBinding(path, "chosen")
	if bindingErr != nil || binding.CredentialFile != absolute {
		t.Fatal("absolute reference changed or was read")
	}
}

func TestBindingAndLocalLoadRejectTheSameInvalidConfigurations(t *testing.T) {
	for _, data := range []string{`{"version":2,"profiles":{}}`, `{"version":1,"profiles":{"chosen":{}}}`, `{"version":1,"profiles":{},"unknown":true}`, `{"version":1,"version":1,"profiles":{}}`, `{"version":1,"profiles":{"broken-\ud800":{}}}`, `{} {}`} {
		path := filepath.Join(t.TempDir(), "profiles.json")
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		_, local := profile.Load(path, "chosen")
		_, bound := profile.LoadBinding(path, "chosen")
		if local == nil || bound == nil || local.Code != bound.Code || local.Message != bound.Message || local.Action != bound.Action {
			t.Fatal("binding changed strict profile diagnostics")
		}
	}
}
