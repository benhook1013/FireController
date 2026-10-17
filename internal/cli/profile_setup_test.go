package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/benhook1013/FireController/internal/cli"
)

func setupPrivateDirectory(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	return root
}

func profileCreateArgs(path, name, server string) []string {
	return []string{"--profiles-file", path, "--profile", name, "profile", "create", "--server", server, "--instance", "instance-local", "--project", "project-local", "--worker", "worker-local", "--credential-file", "credentials/never-created.token"}
}

func setupResult(t *testing.T, code int, out, err string, state, name string) {
	t.Helper()
	var result map[string]any
	if code != cli.ExitSuccess || err != "" || json.Unmarshal([]byte(out), &result) != nil || len(out) > 4<<10 {
		t.Fatalf("setup failed: code=%d out=%q err=%q", code, out, err)
	}
	if result["state"] != state || result["profile"] != name || result["verified"] != false || len(result) != 7 {
		t.Fatalf("unexpected local setup facts: %#v", result)
	}
	if strings.Contains(out, "credential") || strings.Contains(out, "never-created") {
		t.Fatalf("setup leaked private reference: %s", out)
	}
}

func TestProfileSetupCreateLocalRoundTripAndNoWriteReplay(t *testing.T) {
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1); w.WriteHeader(500) }))
	defer server.Close()
	path := filepath.Join(t.TempDir(), "private", "profiles.json")
	args := profileCreateArgs(path, "builder", server.URL)
	code, out, stderr := run(args, map[string]string{"FC_PROFILE": "other"})
	setupResult(t, code, out, stderr, "configured", "builder")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	code, replayOut, replayErr := run(args, nil)
	setupResult(t, code, replayOut, replayErr, "configured", "builder")
	after, _ := os.ReadFile(path)
	afterInfo, _ := os.Stat(path)
	if !bytes.Equal(before, after) || !info.ModTime().Equal(afterInfo.ModTime()) || info.Mode().Perm() != 0600 {
		t.Fatal("replay rewrote profile or configuration is not private")
	}
	code, contextOut, contextErr := run([]string{"--profiles-file", path, "--profile", "builder", "context"}, nil)
	identity := response(t, code, contextOut, contextErr)
	if identity["project_id"] != "project-local" || identity["worker_id"] != "worker-local" {
		t.Fatal(identity)
	}
	conflict := append([]string(nil), args...)
	for i, value := range conflict {
		if value == "worker-local" {
			conflict[i] = "another-worker"
		}
	}
	failure(t, conflict, nil, cli.ExitConflict, "profiles_conflict")
	after, _ = os.ReadFile(path)
	if !bytes.Equal(before, after) || requests.Load() != 0 {
		t.Fatal("conflict changed config or local setup contacted server")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(path), "credentials")); !os.IsNotExist(err) {
		t.Fatal("credential reference was created or inspected as setup state")
	}
}

func TestProfileSetupArgumentsAndCancellationBeforeFilesystem(t *testing.T) {
	root := filepath.Join(t.TempDir(), "must-not-exist")
	path := filepath.Join(root, "profiles.json")
	valid := profileCreateArgs(path, "builder", "https://example.invalid")
	without := func(flag string) []string {
		var result []string
		for i := 0; i < len(valid); i++ {
			if valid[i] == flag {
				i++
				continue
			}
			result = append(result, valid[i])
		}
		return result
	}
	cases := [][]string{
		without("--profile"), without("--profiles-file"), without("--worker"),
		append(append([]string(nil), valid...), "--worker", "another"),
		append(append([]string(nil), valid...), "--unknown=value"),
		append(append([]string(nil), valid...), "extra"),
		append(append([]string(nil), valid...), "--profile", "another"),
	}
	for _, replacement := range []struct{ old, new string }{{path, "relative.json"}, {"builder", "\xff"}, {"worker-local", "bad\x00worker"}, {"credentials/never-created.token", ""}} {
		args := append([]string(nil), valid...)
		for i, v := range args {
			if v == replacement.old {
				args[i] = replacement.new
			}
		}
		cases = append(cases, args)
	}
	for i, args := range cases {
		code, out, err := run(args, map[string]string{"FC_PROFILE": "ambient", "XDG_CONFIG_HOME": root})
		if code != cli.ExitUsage || out != "" || !strings.Contains(err, `"code":"invalid_arguments"`) {
			t.Fatalf("case %d: %d %q %q", i, code, out, err)
		}
		if _, err := os.Stat(root); !os.IsNotExist(err) {
			t.Fatalf("case %d touched filesystem", i)
		}
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	var out, stderr bytes.Buffer
	if code := cli.RunContext(canceled, valid, nil, &out, &stderr); code != cli.ExitCancelled || out.Len() != 0 {
		t.Fatalf("cancel: %d %s %s", code, &out, &stderr)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatal("cancelled setup published files")
	}
	oversize := profileCreateArgs(path, strings.Repeat("<", 800), "https://example.invalid")
	failure(t, oversize, nil, cli.ExitConfig, "setup_output_too_large")
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatal("oversized result published files")
	}
}

func TestProfileSetupPreservesExistingMultiProfileConfig(t *testing.T) {
	path := fixture(t, encode(t, profiles()))
	if err := os.Chmod(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(path)
	args := []string{"--profiles-file", path, "--profile", "alpha-overseer", "profile", "create", "--server", "http://127.0.0.1:9410", "--instance", "instance-alpha", "--project", "project-alpha", "--worker", "worker-alpha", "--credential-file", privateReference}
	// Use the exact existing binding rather than assuming fixture identities.
	var config struct {
		Profiles map[string]map[string]string `json:"profiles"`
	}
	_ = json.Unmarshal(before, &config)
	entry := config.Profiles["alpha-overseer"]
	flags := map[string]string{"--server": "server_url", "--instance": "instance_id", "--project": "project_id", "--worker": "worker_id", "--credential-file": "credential_file"}
	for i := range args {
		if key, ok := flags[args[i]]; ok {
			args[i+1] = entry[key]
		}
	}
	code, out, err := run(args, nil)
	setupResult(t, code, out, err, "configured", "alpha-overseer")
	args[3] = "new-profile"
	failure(t, args, nil, cli.ExitConflict, "profiles_conflict")
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Fatal("multiprofile config changed")
	}
}

type setupFailingWriter struct{}

func (setupFailingWriter) Write([]byte) (int, error) { return 0, errors.New("private output failure") }

func TestProfileSetupOutputFailureRemainsRecoverable(t *testing.T) {
	path := filepath.Join(setupPrivateDirectory(t), "profiles.json")
	args := profileCreateArgs(path, "builder", "https://example.invalid")
	var stderr bytes.Buffer
	if code := cli.Run(args, nil, setupFailingWriter{}, &stderr); code != cli.ExitConfig {
		t.Fatal(code)
	}
	var result struct {
		Error struct {
			Code           string `json:"code"`
			OutcomeUnknown bool   `json:"outcome_unknown"`
			Action         string `json:"action"`
		} `json:"error"`
	}
	if json.Unmarshal(stderr.Bytes(), &result) != nil || result.Error.Code != "output_failed" || !result.Error.OutcomeUnknown || !strings.Contains(result.Error.Action, "exact same command") || strings.Contains(stderr.String(), "private output failure") {
		t.Fatalf("bad uncertainty: %s", &stderr)
	}
	code, out, err := run(args, nil)
	setupResult(t, code, out, err, "configured", "builder")
}

func TestProfileSetupLauncherBindsSelectedIdentityAndForwardsLiteralArguments(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX launcher boundary")
	}
	root := setupPrivateDirectory(t)
	path := filepath.Join(root, "profiles ' literal.json")
	for _, name := range []string{"alpha ' literal", "beta"} {
		selected := path
		if name == "beta" {
			selected = filepath.Join(root, "beta.json")
		}
		code, out, err := run(profileCreateArgs(selected, name, "https://example.invalid"), nil)
		setupResult(t, code, out, err, "configured", name)
	}
	executable := filepath.Join(root, "argument ' recorder")
	if err := os.WriteFile(executable, []byte("#!/bin/sh\nprintf '%s\\000' \"$@\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(root, "fc ' alpha")
	args := []string{"--profiles-file", path, "profile", "launcher", "--output", output, "--executable", executable}
	code, out, stderr := run(args, map[string]string{"FC_PROFILE": "alpha ' literal"})
	setupResult(t, code, out, stderr, "launcher_created", "alpha ' literal")
	before, _ := os.ReadFile(output)
	info, _ := os.Stat(output)
	code, out, stderr = run(args, map[string]string{"FC_PROFILE": "alpha ' literal"})
	setupResult(t, code, out, stderr, "launcher_created", "alpha ' literal")
	after, _ := os.ReadFile(output)
	afterInfo, _ := os.Stat(output)
	if !bytes.Equal(before, after) || !info.ModTime().Equal(afterInfo.ModTime()) || info.Mode().Perm()&0100 == 0 {
		t.Fatal("launcher replay rewrote or lost executable mode")
	}
	forwarded, err := exec.Command(output, "context").Output()
	if err != nil {
		t.Fatal(err)
	}
	bound := strings.Split(strings.TrimSuffix(string(forwarded), "\x00"), "\x00")
	code, out, stderr = run(bound, map[string]string{"FC_PROFILE": "beta"})
	identity := response(t, code, out, stderr)
	if identity["profile"] != "alpha ' literal" {
		t.Fatal("ambient profile changed generated binding")
	}
	forwarded, err = exec.Command(output, "--profile", "beta", "context").Output()
	if err != nil {
		t.Fatal(err)
	}
	bound = strings.Split(strings.TrimSuffix(string(forwarded), "\x00"), "\x00")
	failure(t, bound, nil, cli.ExitUsage, "invalid_arguments")
	conflict := append([]string(nil), args...)
	conflict[len(conflict)-1] = "/bin/sh"
	failure(t, conflict, map[string]string{"FC_PROFILE": "alpha ' literal"}, cli.ExitConflict, "launcher_conflict")
	after, _ = os.ReadFile(output)
	if !bytes.Equal(before, after) {
		t.Fatal("launcher conflict changed bytes")
	}
	var outputErr bytes.Buffer
	if code := cli.Run(args, map[string]string{"FC_PROFILE": "alpha ' literal"}, setupFailingWriter{}, &outputErr); code != cli.ExitConfig || !strings.Contains(outputErr.String(), `"outcome_unknown":true`) {
		t.Fatalf("launcher output uncertainty: %d %s", code, &outputErr)
	}
}

func TestProfileSetupLauncherRoundTripsOptionLikeAndLiteralNames(t *testing.T) {
	if runtime.GOOS == "windows" || runtime.GOOS == "plan9" {
		t.Skip("POSIX launcher boundary")
	}
	for _, test := range []struct{ label, name string }{
		{"option-like", "--ops"},
		{"literal", "--ops ' $(exit 91) `exit 92` =literal"},
	} {
		t.Run(test.label, func(t *testing.T) {
			var requests atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				w.WriteHeader(http.StatusInternalServerError)
			}))
			defer server.Close()
			root := setupPrivateDirectory(t)
			path := filepath.Join(root, "profiles ' literal.json")
			env := map[string]string{"FC_PROFILE": "ambient-other", "XDG_CONFIG_HOME": filepath.Join(root, "unused")}
			create := []string{"--profiles-file", path, "--profile=" + test.name, "profile", "create", "--server", server.URL, "--instance", "instance-local", "--project", "project-local", "--worker", "worker-local", "--credential-file", "credentials/never-created.token"}
			code, out, stderr := run(create, env)
			setupResult(t, code, out, stderr, "configured", test.name)
			executable := filepath.Join(root, "argument ' recorder")
			if err := os.WriteFile(executable, []byte("#!/bin/sh\nprintf '%s\\000' \"$@\"\n"), 0700); err != nil {
				t.Fatal(err)
			}
			output := filepath.Join(root, "fc-selected")
			launcher := []string{"--profiles-file", path, "--profile=" + test.name, "profile", "launcher", "--output", output, "--executable", executable}
			code, out, stderr = run(launcher, env)
			setupResult(t, code, out, stderr, "launcher_created", test.name)
			before, err := os.ReadFile(output)
			if err != nil {
				t.Fatal(err)
			}
			info, err := os.Stat(output)
			if err != nil {
				t.Fatal(err)
			}
			code, out, stderr = run(launcher, env)
			setupResult(t, code, out, stderr, "launcher_created", test.name)
			after, err := os.ReadFile(output)
			if err != nil {
				t.Fatal(err)
			}
			afterInfo, err := os.Stat(output)
			if err != nil || !bytes.Equal(before, after) || !os.SameFile(info, afterInfo) || !info.ModTime().Equal(afterInfo.ModTime()) {
				t.Fatal("exact retry changed launcher")
			}
			invoke := func(args ...string) []string {
				t.Helper()
				cmd := exec.Command(output, args...)
				cmd.Env = append(os.Environ(), "FC_PROFILE=ambient-other")
				forwarded, err := cmd.Output()
				if err != nil {
					t.Fatalf("literal shell invocation: %v", err)
				}
				return strings.Split(strings.TrimSuffix(string(forwarded), "\x00"), "\x00")
			}
			// Exercise the real CLI parser with the actual shell's emitted argv,
			// rather than accepting a correctly quoted but unusable launcher.
			code, out, stderr = run(invoke("context"), env)
			identity := response(t, code, out, stderr)
			if identity["profile"] != test.name || identity["instance_id"] != "instance-local" || identity["project_id"] != "project-local" || identity["worker_id"] != "worker-local" {
				t.Fatalf("launcher resolved another binding: %#v", identity)
			}
			for _, overrides := range [][]string{
				{"--profile", "other", "context"},
				{"--profile=other", "context"},
				{"--profiles-file", path, "context"},
			} {
				failure(t, invoke(overrides...), env, cli.ExitUsage, "invalid_arguments")
			}
			if requests.Load() != 0 {
				t.Fatal("local launcher/context contacted service")
			}
			if _, err := os.Stat(filepath.Join(root, "credentials")); !os.IsNotExist(err) {
				t.Fatal("local setup created credential state")
			}
		})
	}
}

func TestProfileSetupLauncherDefaultSelectionPreflightAndCancellation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX launcher boundary")
	}
	root := setupPrivateDirectory(t)
	profilesPath := filepath.Join(root, "firecontroller", "profiles.json")
	code, out, stderr := run(profileCreateArgs(profilesPath, "builder", "https://example.invalid"), nil)
	setupResult(t, code, out, stderr, "configured", "builder")
	output := filepath.Join(root, "fc-builder")
	env := map[string]string{"FC_PROFILE": "builder", "XDG_CONFIG_HOME": root}
	args := []string{"profile", "launcher", "--output", output}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	var canceledOut, canceledErr bytes.Buffer
	if code := cli.RunContext(canceled, args, env, &canceledOut, &canceledErr); code != cli.ExitCancelled || canceledOut.Len() != 0 {
		t.Fatalf("launcher cancellation: %d %s", code, &canceledErr)
	}
	if _, err := os.Stat(output); !os.IsNotExist(err) {
		t.Fatal("cancellation published launcher")
	}
	code, out, stderr = run(args, env)
	setupResult(t, code, out, stderr, "launcher_created", "builder")
	body, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(body, []byte(executable)) {
		t.Fatal("default launcher did not bind the current executable")
	}
	for _, invalid := range [][]string{
		{"profile", "launcher", "--output", "relative"},
		{"profile", "launcher", "--output", filepath.Join(root, "not-created"), "--executable", "relative"},
		{"profile", "launcher", "--output", filepath.Join(root, "not-created"), "--output", output},
		{"profile", "launcher", "--output", filepath.Join(root, "not-created"), "--unknown", "x"},
		{"profile", "launcher", "--output", filepath.Join(root, "not-created"), "--executable"},
	} {
		failure(t, invalid, env, cli.ExitUsage, "invalid_arguments")
	}
	if _, err := os.Stat(filepath.Join(root, "not-created")); !os.IsNotExist(err) {
		t.Fatal("invalid arguments published launcher")
	}
	// Existing profiles may be larger than the setup output budget. Loading the
	// local context must reject that result before publishing any launcher.
	largePath := fixture(t, encode(t, map[string]any{"version": 1, "profiles": map[string]any{"builder": map[string]string{"server_url": "https://example.invalid", "instance_id": strings.Repeat("<", 800), "project_id": "p", "worker_id": "w", "credential_file": "absent"}}}))
	failure(t, []string{"--profiles-file", largePath, "--profile", "builder", "profile", "launcher", "--output", filepath.Join(root, "too-large")}, nil, cli.ExitConfig, "setup_output_too_large")
	if _, err := os.Stat(filepath.Join(root, "too-large")); !os.IsNotExist(err) {
		t.Fatal("oversized launcher output published file")
	}
}
