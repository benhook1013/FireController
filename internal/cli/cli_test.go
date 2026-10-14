package cli_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/cli"
)

const privateReference = "private-credential-reference.token"

func profiles() map[string]any {
	return map[string]any{
		"version": 1,
		"profiles": map[string]any{
			"alpha-overseer": map[string]any{"server_url": "http://127.0.0.1:8080", "instance_id": "instance-a", "project_id": "alpha-project", "worker_id": "overseer", "credential_file": privateReference},
			"beta-overseer":  map[string]any{"server_url": "https://controller.example", "instance_id": "instance-b", "project_id": "beta-project", "worker_id": "overseer", "credential_file": "../" + privateReference},
		},
	}
}

func encode(t *testing.T, config map[string]any) []byte {
	t.Helper()
	data, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func fixture(t *testing.T, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "profiles.json")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func run(args []string, env map[string]string) (int, string, string) {
	var stdout, stderr bytes.Buffer
	code := cli.Run(args, env, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func response(t *testing.T, code int, stdout, stderr string) map[string]string {
	t.Helper()
	if code != cli.ExitSuccess || stderr != "" {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	var result map[string]string
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatal(err)
	}
	if len(result) != 5 {
		t.Fatalf("unexpected context fields: %v", result)
	}
	for _, key := range []string{"profile", "server_url", "instance_id", "project_id", "worker_id"} {
		if result[key] == "" {
			t.Fatalf("missing context field %s", key)
		}
	}
	if strings.Contains(stdout, privateReference) || strings.Contains(stdout, "credential") {
		t.Fatal("credential reference leaked")
	}
	return result
}

func failure(t *testing.T, args []string, env map[string]string, wantExit int, wantCode string) {
	t.Helper()
	code, stdout, stderr := run(args, env)
	if code != wantExit || stdout != "" {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	var result struct {
		Error struct{ Code, Message, Action string }
	}
	if err := json.Unmarshal([]byte(stderr), &result); err != nil {
		t.Fatalf("invalid error JSON: %v: %q", err, stderr)
	}
	if result.Error.Code != wantCode || result.Error.Message == "" || result.Error.Action == "" {
		t.Fatalf("unexpected diagnostic: %s", stderr)
	}
	if strings.Contains(stderr, privateReference) || strings.Contains(stderr, "SUPER_SECRET_TOKEN") {
		t.Fatalf("private data leaked: %s", stderr)
	}
}

func TestInterleavedProfilesDoNotChangeIdentityOrConfiguration(t *testing.T) {
	data := encode(t, profiles())
	path := fixture(t, data)
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	// Credential references deliberately point at absent files: a local context
	// read must succeed without opening a token or contacting these servers.
	for _, name := range []string{"alpha-overseer", "beta-overseer", "alpha-overseer", "beta-overseer"} {
		code, stdout, stderr := run([]string{"--profile", name, "--profiles-file", path, "context"}, nil)
		got := response(t, code, stdout, stderr)
		wantProject, wantInstance := "alpha-project", "instance-a"
		if name == "beta-overseer" {
			wantProject, wantInstance = "beta-project", "instance-b"
		}
		if got["profile"] != name || got["project_id"] != wantProject || got["instance_id"] != wantInstance || got["worker_id"] != "overseer" {
			t.Fatalf("wrong identity: %v", got)
		}
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	actual, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, actual) || !before.ModTime().Equal(after.ModTime()) || before.Mode() != after.Mode() {
		t.Fatal("context changed configuration")
	}
	files, err := os.ReadDir(filepath.Dir(path))
	if err != nil || len(files) != 1 {
		t.Fatalf("unexpected configuration writes: files=%v err=%v", files, err)
	}
}

func TestProfilePrecedenceAndFlagsOnEitherSideOfCommand(t *testing.T) {
	path := fixture(t, encode(t, profiles()))
	env := map[string]string{"FC_PROFILE": "beta-overseer"}
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"context", "--profiles-file", path}, "beta-overseer"},
		{[]string{"--profile=alpha-overseer", "context", "--profiles-file=" + path}, "alpha-overseer"},
		{[]string{"context", "--profile", "alpha-overseer", "--profiles-file", path}, "alpha-overseer"},
	} {
		code, stdout, stderr := run(tc.args, env)
		if got := response(t, code, stdout, stderr); got["profile"] != tc.want {
			t.Fatalf("wrong profile: %v", got)
		}
	}
	if env["FC_PROFILE"] != "beta-overseer" {
		t.Fatal("environment map mutated")
	}
	// An invalid explicit selector must not fall back to a valid environment.
	failure(t, []string{"context", "--profiles-file", path, "--profile", "missing"}, env, cli.ExitConfig, "profile_not_found")
}

func TestDefaultConfigPathUsesExplicitUserEnvironment(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux configuration directory contract")
	}
	for _, useXDG := range []bool{true, false} {
		t.Run(map[bool]string{true: "xdg", false: "home"}[useXDG], func(t *testing.T) {
			directory := t.TempDir()
			env := map[string]string{"HOME": directory, "FC_PROFILE": "alpha-overseer"}
			configDir := filepath.Join(directory, ".config")
			if useXDG {
				configDir = filepath.Join(directory, "xdg")
				env["XDG_CONFIG_HOME"] = configDir
			}
			path := filepath.Join(configDir, "firecontroller", "profiles.json")
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, encode(t, profiles()), 0600); err != nil {
				t.Fatal(err)
			}
			code, stdout, stderr := run([]string{"context"}, env)
			response(t, code, stdout, stderr)
		})
	}
	failure(t, []string{"context"}, map[string]string{"FC_PROFILE": "alpha-overseer"}, cli.ExitConfig, "config_directory_unavailable")
	failure(t, []string{"context"}, map[string]string{"FC_PROFILE": "alpha-overseer", "XDG_CONFIG_HOME": "relative", "HOME": t.TempDir()}, cli.ExitConfig, "config_directory_unavailable")
}

func TestRelativeDefaultDirectoryCannotSelectWorkspaceConfiguration(t *testing.T) {
	env := map[string]string{"FC_PROFILE": "alpha-overseer"}
	switch runtime.GOOS {
	case "windows":
		env["APPDATA"] = "relative"
	case "plan9":
		env["home"] = "relative"
	default:
		env["HOME"] = "relative"
	}
	failure(t, []string{"context"}, env, cli.ExitConfig, "config_directory_unavailable")
}

func TestVersionDoesNotNeedOrReadConfiguration(t *testing.T) {
	code, stdout, stderr := run([]string{"version"}, map[string]string{"FC_PROFILE": "missing"})
	if code != 0 || stderr != "" {
		t.Fatalf("code=%d stderr=%s", code, stderr)
	}
	var result map[string]string
	if err := json.Unmarshal([]byte(stdout), &result); err != nil || len(result) != 1 || result["version"] == "" {
		t.Fatalf("version=%s err=%v", stdout, err)
	}
}

func TestUsageAndSelectionErrorsKeepStdoutClean(t *testing.T) {
	for _, args := range [][]string{nil, {"unknown"}, {"context", "extra"}, {"context", "--unknown"}, {"context", "--profile"}, {"context", "--profiles-file="}, {"context", "--profile", "a", "--profile", "b"}, {"context", "--profiles-file", "a", "--profiles-file", "b"}, {"version", "--profile", "a"}, {"context", "--profile", "--profiles-file", "a"}} {
		failure(t, args, nil, cli.ExitUsage, "invalid_arguments")
	}
	failure(t, []string{"context"}, nil, cli.ExitConfig, "profile_required")
	failure(t, []string{"context", "--profile", " alpha-overseer "}, nil, cli.ExitConfig, "profile_required")
	failure(t, []string{"context", "--profile", "alpha-overseer", "--profiles-file", filepath.Join(t.TempDir(), privateReference)}, nil, cli.ExitConfig, "profiles_unreadable")
}

func TestStrictConfigurationFailures(t *testing.T) {
	valid := string(encode(t, profiles()))
	for _, tc := range []struct{ name, data, code string }{
		{"malformed", `{"version":`, "profiles_invalid_json"},
		{"trailing object", valid + `{}`, "profiles_invalid_json"},
		{"trailing text", valid + `SUPER_SECRET_TOKEN`, "profiles_invalid_json"},
		{"unknown root field", strings.Replace(valid, `"version":1`, `"version":1,"SUPER_SECRET_TOKEN":"`+privateReference+`"`, 1), "profiles_invalid_json"},
		{"duplicate version", strings.Replace(valid, `"version":1`, `"version":1,"version":1`, 1), "profiles_invalid_json"},
		{"duplicate profile", `{"version":1,"profiles":{"same":{},"same":{}}}`, "profiles_invalid_json"},
		{"duplicate identity", strings.Replace(valid, `"worker_id":"overseer"`, `"worker_id":"overseer","worker_id":"other"`, 1), "profiles_invalid_json"},
		{"case variant identity", strings.Replace(valid, `"worker_id":"overseer"`, `"worker_id":"overseer","WORKER_ID":"other"`, 1), "profiles_invalid_json"},
		{"case variant schema", strings.Replace(valid, `"version":1`, `"Version":1`, 1), "profiles_invalid_json"},
		{"unknown entry field", strings.Replace(valid, `"worker_id":"overseer"`, `"worker_id":"overseer","SUPER_SECRET_TOKEN":"`+privateReference+`"`, 1), "profiles_invalid_json"},
		{"unsupported version", strings.Replace(valid, `"version":1`, `"version":2`, 1), "unsupported_profiles_version"},
		{"missing version", `{"profiles":{}}`, "unsupported_profiles_version"},
		{"missing profiles", `{"version":1}`, "profiles_invalid"},
		{"deep nesting", strings.Repeat("[", 10000) + "0" + strings.Repeat("]", 10000), "profiles_invalid_json"},
		{"oversize", strings.Repeat(" ", (1<<20)+1), "profiles_too_large"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := fixture(t, []byte(tc.data))
			failure(t, []string{"context", "--profile", "alpha-overseer", "--profiles-file", path}, nil, cli.ExitConfig, tc.code)
		})
	}
}

func TestRequiredAndUnambiguousProfileFields(t *testing.T) {
	for _, field := range []string{"instance_id", "project_id", "worker_id", "credential_file"} {
		for _, value := range []any{nil, "", " ", " padded "} {
			config := profiles()
			config["profiles"].(map[string]any)["alpha-overseer"].(map[string]any)[field] = value
			path := fixture(t, encode(t, config))
			failure(t, []string{"context", "--profile", "alpha-overseer", "--profiles-file", path}, nil, cli.ExitConfig, "profiles_invalid")
		}
	}
	for _, name := range []string{"", " ", " alpha-overseer "} {
		config := profiles()
		entries := config["profiles"].(map[string]any)
		entries[name] = entries["alpha-overseer"]
		path := fixture(t, encode(t, config))
		failure(t, []string{"context", "--profile", "alpha-overseer", "--profiles-file", path}, nil, cli.ExitConfig, "profiles_invalid")
	}
}

func TestServerTransportAndURLSafety(t *testing.T) {
	for _, value := range []string{"", " https://controller.example", "ftp://controller.example", "http://controller.example", "http://0.0.0.0", "http://localhost.evil.example", "https://user:SUPER_SECRET_TOKEN@controller.example", "https://controller.example?token=SUPER_SECRET_TOKEN", "https://controller.example?", "https://controller.example#SUPER_SECRET_TOKEN", "https://controller.example#", "https:///path", "https:controller.example", "https://controller.example:bad"} {
		config := profiles()
		config["profiles"].(map[string]any)["alpha-overseer"].(map[string]any)["server_url"] = value
		path := fixture(t, encode(t, config))
		failure(t, []string{"context", "--profile", "alpha-overseer", "--profiles-file", path}, nil, cli.ExitConfig, "profiles_unsafe_url")
	}
	for _, value := range []string{"http://localhost:8080", "http://LOCALHOST", "http://127.8.0.1", "http://[::1]:8080", "https://controller.example/base", "https://192.0.2.1", "HTTPS://controller.example", "HTTP://LOCALHOST:8080"} {
		config := profiles()
		config["profiles"].(map[string]any)["alpha-overseer"].(map[string]any)["server_url"] = value
		path := fixture(t, encode(t, config))
		code, stdout, stderr := run([]string{"context", "--profile", "alpha-overseer", "--profiles-file", path}, nil)
		if got := response(t, code, stdout, stderr); got["server_url"] != value {
			t.Fatalf("wrong URL: %v", got)
		}
	}
}
