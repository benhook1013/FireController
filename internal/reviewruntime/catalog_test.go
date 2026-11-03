package reviewruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/core"
)

const catalogHostedSettings = `{"provider_actor_id":101,"provider_login":"coderabbitai[bot]","progress_mode":"commit_status","progress_app_id":0}`

const publicCatalog = `{"version":1,"capabilities":[{"id":"local-review","account":"included-account","organization":"example","channels":["cli"],"git_executable":"git","cli_executable":"coderabbit","authentication_environment":{"HOME":"/private/account"}}]}`

func TestLoadCatalogDisabledAndPrivateValues(t *testing.T) {
	catalog, err := LoadCatalog("")
	if err != nil {
		t.Fatal(err)
	}
	if catalog.Version != 1 || catalog.Capabilities == nil || len(catalog.Capabilities) != 0 {
		t.Fatalf("disabled catalog = %#v", catalog)
	}

	dir := t.TempDir()
	token := "synthetic-token-value\n"
	writePrivate(t, filepath.Join(dir, "github.token"), []byte(token))
	path := writeCatalog(t, dir, `{"version":1,"capabilities":[{"id":"hosted","account":"account","organization":"example.org","channels":["hosted"],"hosted":`+catalogHostedSettings+`,"github_credential_file":"github.token","git_executable":"git"}]}`)
	loaded, err := LoadCatalog(path)
	if err != nil {
		t.Fatal(err)
	}
	capability := loaded.Capabilities[0]
	if capability.GitHubCredential() != "synthetic-token-value" {
		t.Fatalf("credential was not normalized: %q", capability.GitHubCredential())
	}
	encoded, err := json.Marshal(loaded)
	if err != nil {
		t.Fatal(err)
	}
	for _, private := range []string{"synthetic-token-value", "github.token", dir, "/private/account", "coderabbit"} {
		if strings.Contains(string(encoded), private) {
			t.Errorf("JSON exposed private value %q: %s", private, encoded)
		}
	}
	if strings.Contains(loaded.String()+" "+capability.String(), "synthetic-token-value") || strings.Contains(loaded.String()+" "+capability.String(), dir) {
		t.Fatal("String exposed private data")
	}
	formatted := fmt.Sprintf("%#v %#v", loaded, capability)
	for _, private := range []string{"synthetic-token-value", "github.token", dir, "/private/account", "coderabbit", "git"} {
		if strings.Contains(formatted, private) {
			t.Errorf("GoString exposed private value %q: %s", private, formatted)
		}
	}
}

func TestCatalogFingerprintCanonicalAndScoped(t *testing.T) {
	dir := t.TempDir()
	writePrivate(t, filepath.Join(dir, "one.token"), []byte("one-token"))
	writePrivate(t, filepath.Join(dir, "two.token"), []byte("two-token"))
	base := `{"version":1,"capabilities":[{"id":"first","account":"acct","organization":"example","channels":["hosted","cli"],"hosted":` + catalogHostedSettings + `,"github_credential_file":"one.token","git_executable":"git","cli_executable":"coderabbit","authentication_environment":{"HOME":"/private/home"}}]}`
	firstPath := writeCatalog(t, dir, base)
	first, err := LoadCatalog(firstPath)
	if err != nil {
		t.Fatal(err)
	}
	if first.Capabilities[0].Channels[0] != "cli" || len(first.Capabilities[0].Fingerprint) != 64 || strings.ToLower(first.Capabilities[0].Fingerprint) != first.Capabilities[0].Fingerprint {
		t.Fatalf("channels/fingerprint are not canonical: %#v", first.Capabilities[0])
	}

	canonicalDifferentOrder := `{
  "capabilities": [{"authentication_environment":{"HOME":"/private/home"},"cli_executable":"coderabbit","git_executable":"git","github_credential_file":"one.token","hosted":` + catalogHostedSettings + `,"channels":["cli","hosted"],"organization":"example","account":"acct","id":"first"}],
  "version": 1
}`
	second, err := LoadCatalog(writeCatalog(t, dir, canonicalDifferentOrder))
	if err != nil {
		t.Fatal(err)
	}
	if second.Capabilities[0].Fingerprint != first.Capabilities[0].Fingerprint {
		t.Fatal("whitespace, key order, or channel order changed canonical fingerprint")
	}

	changedToken, err := LoadCatalog(writeCatalog(t, dir, strings.Replace(base, "one.token", "two.token", 1)))
	if err != nil {
		t.Fatal(err)
	}
	if changedToken.Capabilities[0].Fingerprint == first.Capabilities[0].Fingerprint {
		t.Fatal("credential change did not change fingerprint")
	}
	otherRef := `{"version":1,"capabilities":[{"id":"first","account":"acct","organization":"example","channels":["hosted","cli"],"hosted":` + catalogHostedSettings + `,"github_credential_file":` + quoteJSON(t, filepath.Join(dir, "one.token")) + `,"git_executable":"git","cli_executable":"coderabbit","authentication_environment":{"HOME":"/private/home"}}]}`
	otherReference, err := LoadCatalog(writeCatalog(t, dir, otherRef))
	if err != nil {
		t.Fatal(err)
	}
	if otherReference.Capabilities[0].Fingerprint != first.Capabilities[0].Fingerprint {
		t.Fatal("credential file location changed the effective capability fingerprint")
	}

	extra, err := LoadCatalog(writeCatalog(t, dir, strings.Replace(base, `"capabilities":[`, `"capabilities":[{"id":"other","account":"other","organization":"other","channels":["cli"],"git_executable":"git","cli_executable":"coderabbit"},`, 1)))
	if err != nil {
		t.Fatal(err)
	}
	if extra.Capabilities[1].Fingerprint != first.Capabilities[0].Fingerprint {
		t.Fatal("unrelated capability changed an existing capability fingerprint")
	}
}

func TestLoadCatalogStrictShapesAndValues(t *testing.T) {
	cases := []struct {
		name string
		json string
	}{
		{"missing-version", `{"capabilities":[]}`},
		{"null-version", `{"version":null,"capabilities":[]}`},
		{"wrong-version", `{"version":2,"capabilities":[]}`},
		{"missing-capabilities", `{"version":1}`},
		{"null-capabilities", `{"version":1,"capabilities":null}`},
		{"unknown-field", `{"version":1,"capabilities":[],"extra":true}`},
		{"case-alias", `{"Version":1,"capabilities":[]}`},
		{"duplicate-field", `{"version":1,"version":1,"capabilities":[]}`},
		{"invalid-utf8", string([]byte{'{', '"', 'v', 0xff, '"', '}', '\n'})},
		{"null-required-string", `{"version":1,"capabilities":[{"id":null,"account":"a","organization":"o","channels":["cli"],"git_executable":"git","cli_executable":"tool"}]}`},
		{"null-auth-map", `{"version":1,"capabilities":[{"id":"a","account":"a","organization":"o","channels":["cli"],"git_executable":"git","cli_executable":"tool","authentication_environment":null}]}`},
		{"null-auth-value", `{"version":1,"capabilities":[{"id":"a","account":"a","organization":"o","channels":["cli"],"git_executable":"git","cli_executable":"tool","authentication_environment":{"HOME":null}}]}`},
		{"duplicate-capability-id", `{"version":1,"capabilities":[{"id":"a","account":"a","organization":"o","channels":["cli"],"git_executable":"git","cli_executable":"tool"},{"id":"a","account":"b","organization":"o","channels":["cli"],"git_executable":"git","cli_executable":"tool"}]}`},
		{"bad-id", `{"version":1,"capabilities":[{"id":"bad/id","account":"a","organization":"o","channels":["cli"],"git_executable":"git","cli_executable":"tool"}]}`},
		{"dot-only-org", `{"version":1,"capabilities":[{"id":"a","account":"a","organization":"...","channels":["cli"],"git_executable":"git","cli_executable":"tool"}]}`},
		{"duplicate-channel", `{"version":1,"capabilities":[{"id":"a","account":"a","organization":"o","channels":["cli","cli"],"git_executable":"git","cli_executable":"tool"}]}`},
		{"unsupported-channel", `{"version":1,"capabilities":[{"id":"a","account":"a","organization":"o","channels":["hosted","manual"],"git_executable":"git"}]}`},
		{"missing-cli-executable", `{"version":1,"capabilities":[{"id":"a","account":"a","organization":"o","channels":["cli"],"git_executable":"git"}]}`},
		{"hosted-without-token", `{"version":1,"capabilities":[{"id":"a","account":"a","organization":"o","channels":["hosted"],"git_executable":"git"}]}`},
		{"unknown-auth-key", `{"version":1,"capabilities":[{"id":"a","account":"a","organization":"o","channels":["cli"],"git_executable":"git","cli_executable":"tool","authentication_environment":{"PATH":"/tmp"}}]}`},
		{"relative-auth-path", `{"version":1,"capabilities":[{"id":"a","account":"a","organization":"o","channels":["cli"],"git_executable":"git","cli_executable":"tool","authentication_environment":{"HOME":"relative"}}]}`},
		{"too-many-capabilities", catalogWithCapabilities(33)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeCatalog(t, t.TempDir(), tc.json)
			_, err := LoadCatalog(path)
			if err == nil {
				t.Fatal("expected invalid catalog")
			}
			if strings.Contains(err.Error(), path) || strings.Contains(err.Error(), tc.json) {
				t.Fatalf("unsafe or unexpected error: %v", err)
			}
		})
	}
}

func TestLoadCatalogCredentialValidation(t *testing.T) {
	cases := []struct {
		name  string
		value []byte
	}{
		{"empty", []byte("\n")},
		{"internal-space", []byte("two words")},
		{"control", []byte("token\x7f")},
		{"invalid-utf8", []byte{0xff}},
		{"too-long", []byte(strings.Repeat("x", maxCredentialTokenBytes+1))},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writePrivate(t, filepath.Join(dir, "token"), tc.value)
			path := writeCatalog(t, dir, hostedCatalog("token"))
			if _, err := LoadCatalog(path); err == nil {
				t.Fatal("expected invalid credential")
			}
		})
	}

	dir := t.TempDir()
	writePrivate(t, filepath.Join(dir, "token"), []byte(strings.Repeat("x", maxCredentialBytes+1)))
	if _, err := LoadCatalog(writeCatalog(t, dir, hostedCatalog("token"))); err == nil {
		t.Fatal("oversized credential file accepted")
	}
}

func TestLoadCatalogPrivateFiles(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix mode-bit checks are not available on Windows")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "catalog.json")
	if err := os.WriteFile(path, []byte(`{"version":1,"capabilities":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadCatalog(path); err == nil {
		t.Fatal("group/world-readable catalog accepted")
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadCatalog(path); err != nil {
		t.Fatalf("private catalog rejected: %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadCatalog(path); err == nil {
		t.Fatal("directory accepted as catalog")
	}

	linkDir := t.TempDir()
	regular := writeCatalog(t, linkDir, `{"version":1,"capabilities":[]}`)
	link := filepath.Join(linkDir, "link.json")
	if err := os.Symlink(regular, link); err == nil {
		if _, err := LoadCatalog(link); err == nil {
			t.Fatal("symlink catalog accepted")
		}
	}
}

func TestLoadCatalogFIFORejectsWithoutBlocking(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("FIFO is Unix-specific")
	}
	mkfifo, err := exec.LookPath("mkfifo")
	if err != nil {
		t.Skip("mkfifo unavailable")
	}
	path := filepath.Join(t.TempDir(), "catalog.fifo")
	if out, err := exec.Command(mkfifo, "-m", "600", path).CombinedOutput(); err != nil {
		t.Fatalf("mkfifo: %v: %s", err, out)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestCatalogFIFOHelper$")
	command.Env = append(os.Environ(), "FC_REVIEW_CATALOG_FIFO="+path)
	if err := command.Run(); err != nil {
		if ctx.Err() != nil {
			t.Fatal("LoadCatalog blocked opening a FIFO")
		}
		t.Fatalf("FIFO helper failed: %v", err)
	}
}

func TestCatalogFIFOHelper(t *testing.T) {
	path := os.Getenv("FC_REVIEW_CATALOG_FIFO")
	if path == "" {
		return
	}
	if _, err := LoadCatalog(path); err == nil {
		t.Fatal("FIFO accepted")
	}
}

func TestLoadCatalogRejectsMissingOversizedAndUnsafeSecret(t *testing.T) {
	dir := t.TempDir()
	if _, err := LoadCatalog(filepath.Join(dir, "missing")); err == nil {
		t.Fatal("missing catalog accepted")
	}
	large := writeCatalog(t, dir, strings.Repeat(" ", maxCatalogBytes+1))
	if _, err := LoadCatalog(large); err == nil {
		t.Fatal("oversized catalog accepted")
	}
	if runtime.GOOS != "windows" {
		writePrivate(t, filepath.Join(dir, "token"), []byte("do-not-disclose"))
		if err := os.Chmod(filepath.Join(dir, "token"), 0o644); err != nil {
			t.Fatal(err)
		}
		path := writeCatalog(t, dir, hostedCatalog("token"))
		if _, err := LoadCatalog(path); err == nil {
			t.Fatal("group/world-readable credential accepted")
		} else if strings.Contains(err.Error(), "do-not-disclose") || strings.Contains(err.Error(), path) {
			t.Fatalf("configuration error disclosed private data: %v", err)
		}
	}
}

func writeCatalog(t *testing.T, dir, content string) string {
	t.Helper()
	path := filepath.Join(dir, "catalog.json")
	writePrivate(t, path, []byte(content))
	return path
}

func writePrivate(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func hostedCatalog(reference string) string {
	return `{"version":1,"capabilities":[{"id":"hosted","account":"account","organization":"example","channels":["hosted"],"hosted":` + catalogHostedSettings + `,"github_credential_file":` + quoteJSON(nil, reference) + `,"git_executable":"git"}]}`
}

func catalogWithCapabilities(count int) string {
	var capabilities []string
	for i := 0; i < count; i++ {
		capabilities = append(capabilities, `{"id":"id-`+strconv.Itoa(i)+`","account":"acct","organization":"org","channels":["cli"],"git_executable":"git","cli_executable":"tool"}`)
	}
	return `{"version":1,"capabilities":[` + strings.Join(capabilities, ",") + `]}`
}

func quoteJSON(t *testing.T, value string) string {
	if t != nil {
		t.Helper()
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		if t != nil {
			t.Fatal(err)
		}
		panic("string JSON encoding failed")
	}
	return string(encoded)
}

func TestCatalogFailureIsSafeCoreConfigurationError(t *testing.T) {
	path := writeCatalog(t, t.TempDir(), `{"version":1,"capabilities":null}`)
	_, err := LoadCatalog(path)
	if err == nil {
		t.Fatal("expected failure")
	}
	var coded *core.Error
	if !errors.As(err, &coded) {
		t.Fatalf("error lacks stable code: %T %v", err, err)
	}
	if coded.Code != "configuration_invalid" {
		t.Fatalf("error code = %q", coded.Code)
	}
}

func TestCatalogHostedSettingsStrictShapesAndChannelBindings(t *testing.T) {
	dir := t.TempDir()
	writePrivate(t, filepath.Join(dir, "token"), []byte("synthetic-hosted-token"))
	base := hostedCatalog("token")
	for name, settings := range map[string]string{
		"missing actor":        `{"provider_login":"coderabbitai[bot]","progress_mode":"commit_status"}`,
		"null actor":           `{"provider_actor_id":null,"provider_login":"coderabbitai[bot]","progress_mode":"commit_status"}`,
		"zero actor":           `{"provider_actor_id":0,"provider_login":"coderabbitai[bot]","progress_mode":"commit_status"}`,
		"negative actor":       `{"provider_actor_id":-1,"provider_login":"coderabbitai[bot]","progress_mode":"commit_status"}`,
		"fractional actor":     `{"provider_actor_id":1.5,"provider_login":"coderabbitai[bot]","progress_mode":"commit_status"}`,
		"string actor":         `{"provider_actor_id":"1","provider_login":"coderabbitai[bot]","progress_mode":"commit_status"}`,
		"overflow actor":       `{"provider_actor_id":9223372036854775808,"provider_login":"coderabbitai[bot]","progress_mode":"commit_status"}`,
		"missing login":        `{"provider_actor_id":1,"progress_mode":"commit_status"}`,
		"null login":           `{"provider_actor_id":1,"provider_login":null,"progress_mode":"commit_status"}`,
		"empty login":          `{"provider_actor_id":1,"provider_login":"","progress_mode":"commit_status"}`,
		"whitespace login":     `{"provider_actor_id":1,"provider_login":" coderabbitai[bot]","progress_mode":"commit_status"}`,
		"newline login":        `{"provider_actor_id":1,"provider_login":"coderabbitai\n","progress_mode":"commit_status"}`,
		"unicode login":        `{"provider_actor_id":1,"provider_login":"cöderabbitai","progress_mode":"commit_status"}`,
		"malformed bot suffix": `{"provider_actor_id":1,"provider_login":"coderabbitai[Bot]","progress_mode":"commit_status"}`,
		"suffix only":          `{"provider_actor_id":1,"provider_login":"[bot]","progress_mode":"commit_status"}`,
		"long login":           `{"provider_actor_id":1,"provider_login":"` + strings.Repeat("a", 101) + `","progress_mode":"commit_status"}`,
		"missing mode":         `{"provider_actor_id":1,"provider_login":"coderabbitai[bot]"}`,
		"null mode":            `{"provider_actor_id":1,"provider_login":"coderabbitai[bot]","progress_mode":null}`,
		"unknown mode":         `{"provider_actor_id":1,"provider_login":"coderabbitai[bot]","progress_mode":"automatic"}`,
		"mode alias":           `{"provider_actor_id":1,"provider_login":"coderabbitai[bot]","progress_mode":"CHECK_RUN","progress_app_id":2}`,
		"status with app":      `{"provider_actor_id":1,"provider_login":"coderabbitai[bot]","progress_mode":"commit_status","progress_app_id":2}`,
		"status negative app":  `{"provider_actor_id":1,"provider_login":"coderabbitai[bot]","progress_mode":"commit_status","progress_app_id":-1}`,
		"null app":             `{"provider_actor_id":1,"provider_login":"coderabbitai[bot]","progress_mode":"commit_status","progress_app_id":null}`,
		"check missing app":    `{"provider_actor_id":1,"provider_login":"coderabbitai[bot]","progress_mode":"check_run"}`,
		"check zero app":       `{"provider_actor_id":1,"provider_login":"coderabbitai[bot]","progress_mode":"check_run","progress_app_id":0}`,
		"check negative app":   `{"provider_actor_id":1,"provider_login":"coderabbitai[bot]","progress_mode":"check_run","progress_app_id":-1}`,
		"unknown field":        strings.TrimSuffix(catalogHostedSettings, "}") + `,"provider_kind":"Bot"}`,
		"case alias":           strings.Replace(catalogHostedSettings, `"provider_actor_id"`, `"Provider_Actor_ID"`, 1),
		"duplicate field":      strings.TrimSuffix(catalogHostedSettings, "}") + `,"provider_actor_id":101}`,
		"null settings":        `null`,
		"array settings":       `[]`,
	} {
		t.Run(name, func(t *testing.T) {
			raw := strings.Replace(base, catalogHostedSettings, settings, 1)
			_, err := LoadCatalog(writeCatalog(t, dir, raw))
			if err == nil {
				t.Fatal("invalid hosted settings accepted")
			}
			if strings.Contains(err.Error(), raw) || strings.Contains(err.Error(), dir) || strings.Contains(err.Error(), "synthetic-hosted-token") {
				t.Fatal("invalid settings disclosed private source", err)
			}
		})
	}
	for name, raw := range map[string]string{
		"hosted missing settings":   strings.Replace(base, `"hosted":`+catalogHostedSettings+`,`, "", 1),
		"hosted missing credential": strings.Replace(base, `"github_credential_file":"token",`, "", 1),
		"CLI only with settings":    strings.Replace(publicCatalog, `"channels":["cli"]`, `"channels":["cli"],"hosted":`+catalogHostedSettings, 1),
		"hosted field alias":        strings.Replace(base, `"hosted":`, `"Hosted":`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := LoadCatalog(writeCatalog(t, dir, raw)); err == nil {
				t.Fatal("invalid hosted channel binding accepted")
			}
		})
	}
	if validHostedSettings(nil) {
		t.Fatal("nil settings accepted by constructor validator")
	}
}

func TestCatalogHostedOnlySettingsFingerprintAndPrivateSerialization(t *testing.T) {
	dir := t.TempDir()
	writePrivate(t, filepath.Join(dir, "token"), []byte("synthetic-hosted-token"))
	base := hostedCatalog("token")
	loaded, err := LoadCatalog(writeCatalog(t, dir, base))
	if err != nil {
		t.Fatal(err)
	}
	capability := loaded.Capabilities[0]
	if capability.Hosted == nil || capability.Hosted.ProviderActorID != 101 || capability.Hosted.ProviderLogin != "coderabbitai[bot]" || capability.Hosted.ProgressMode != "commit_status" || capability.Hosted.ProgressAppID != 0 || capability.CLIExecutable != "" || len(capability.AuthenticationEnvironment) != 0 || !validHostedSettings(capability.Hosted) {
		t.Fatal("hosted-only catalog lost settings or required CLI fields")
	}
	absentZero := strings.Replace(base, `,"progress_app_id":0`, "", 1)
	reordered := strings.Replace(base, catalogHostedSettings, `{"progress_mode":"commit_status","provider_login":"coderabbitai[bot]","provider_actor_id":101}`, 1)
	for _, raw := range []string{absentZero, reordered} {
		got, err := LoadCatalog(writeCatalog(t, dir, raw))
		if err != nil || got.Capabilities[0].Fingerprint != capability.Fingerprint {
			t.Fatal("equivalent hosted settings changed fingerprint", err)
		}
	}
	for name, settings := range map[string]string{
		"actor": strings.Replace(catalogHostedSettings, `"provider_actor_id":101`, `"provider_actor_id":102`, 1),
		"login": strings.Replace(catalogHostedSettings, `coderabbitai[bot]`, `CodeRabbitAI[bot]`, 1),
		"mode":  `{"provider_actor_id":101,"provider_login":"coderabbitai[bot]","progress_mode":"check_run","progress_app_id":7}`,
	} {
		t.Run(name, func(t *testing.T) {
			got, err := LoadCatalog(writeCatalog(t, dir, strings.Replace(base, catalogHostedSettings, settings, 1)))
			if err != nil || got.Capabilities[0].Fingerprint == capability.Fingerprint {
				t.Fatal("hosted setting failed to bind fingerprint", err)
			}
			if name == "login" && got.Capabilities[0].Hosted.ProviderLogin != "CodeRabbitAI[bot]" {
				t.Fatal("display login silently normalized")
			}
			if name == "mode" {
				changed, err := LoadCatalog(writeCatalog(t, dir, strings.Replace(strings.Replace(base, catalogHostedSettings, settings, 1), `"progress_app_id":7`, `"progress_app_id":8`, 1)))
				if err != nil || changed.Capabilities[0].Fingerprint == got.Capabilities[0].Fingerprint {
					t.Fatal("progress app change failed to bind fingerprint", err)
				}
			}
		})
	}
	encoded, err := json.Marshal(loaded)
	if err != nil {
		t.Fatal(err)
	}
	formatted := fmt.Sprintf("%v %+v %#v %#v", loaded, capability, capability, *capability.Hosted)
	for _, private := range []string{"provider_actor_id", "provider_login", "progress_mode", "progress_app_id", "coderabbitai[bot]", "commit_status", "synthetic-hosted-token", dir} {
		if strings.Contains(string(encoded), private) || strings.Contains(formatted, private) {
			t.Fatal("hosted private data leaked through serialization/formatting", private)
		}
	}
}

func TestCatalogCLIOnlyFingerprintUnchangedAndHostedCopiesIndependent(t *testing.T) {
	loaded, err := LoadCatalog(writeCatalog(t, t.TempDir(), publicCatalog))
	if err != nil {
		t.Fatal(err)
	}
	// This exact value predates the optional hosted fingerprint member. Omitting
	// nil settings must preserve existing CLI-only operation capability bindings.
	const beforeHostedSettings = "f7e567144adc2fac285e3217d2a81f009581adf2615cd1f228a5a96a9e6fbf0b"
	if loaded.Capabilities[0].Hosted != nil || loaded.Capabilities[0].Fingerprint != beforeHostedSettings {
		t.Fatal("CLI-only fingerprint changed", loaded.Capabilities[0].Fingerprint)
	}
	dir := t.TempDir()
	writePrivate(t, filepath.Join(dir, "token"), []byte("synthetic-hosted-token"))
	input := capabilityFile{ID: "hosted", Account: "account", Organization: "example", Channels: []string{"hosted"}, GitHubCredentialFile: "token", GitExecutable: "git", Hosted: &HostedSettings{ProviderActorID: 101, ProviderLogin: "coderabbitai[bot]", ProgressMode: "commit_status"}}
	first, err := loadCapability(input, dir)
	if err != nil {
		t.Fatal(err)
	}
	second, err := loadCapability(input, dir)
	if err != nil {
		t.Fatal(err)
	}
	if first.Hosted == input.Hosted || second.Hosted == input.Hosted || first.Hosted == second.Hosted {
		t.Fatal("loaded settings alias caller or another capability")
	}
	first.Hosted.ProviderActorID = 999
	first.Channels[0] = "changed caller copy"
	if input.Hosted.ProviderActorID != 101 || second.Hosted.ProviderActorID != 101 || input.Channels[0] != "hosted" || second.Channels[0] != "hosted" {
		t.Fatal("returned settings or channels changed another owner")
	}
	input.Hosted.ProviderLogin = "changed-input"
	if first.Hosted.ProviderLogin != "coderabbitai[bot]" || second.Hosted.ProviderLogin != "coderabbitai[bot]" {
		t.Fatal("input settings changed loaded capability")
	}
}

func TestCatalogCLIAllowanceTimeoutDefaultAndBounds(t *testing.T) {
	dir := t.TempDir()
	baseline, err := LoadCatalog(writeCatalog(t, dir, publicCatalog))
	if err != nil {
		t.Fatal(err)
	}
	old := baseline.Capabilities[0]
	if old.Fingerprint != "f7e567144adc2fac285e3217d2a81f009581adf2615cd1f228a5a96a9e6fbf0b" {
		t.Fatal("default capability fingerprint changed")
	}
	for _, seconds := range []int{1, 15, 30, 60} {
		raw := strings.Replace(publicCatalog, `"cli_executable":"coderabbit"`, `"cli_executable":"coderabbit","cli_allowance_timeout_seconds":`+strconv.Itoa(seconds), 1)
		got, err := LoadCatalog(writeCatalog(t, dir, raw))
		if err != nil {
			t.Fatal(seconds, err)
		}
		c := got.Capabilities[0]
		want := seconds
		if seconds == 15 {
			want = 0
		}
		if c.CLIAllowanceTimeoutSeconds != want || (c.Fingerprint == old.Fingerprint) != (seconds == 15) {
			t.Fatal("timeout did not bind to effective capability", seconds, c.CLIAllowanceTimeoutSeconds)
		}
	}
	for _, bad := range []string{"0", "-1", "61", "1.5", "null", `"30"`, `true`} {
		raw := strings.Replace(publicCatalog, `"cli_executable":"coderabbit"`, `"cli_executable":"coderabbit","cli_allowance_timeout_seconds":`+bad, 1)
		if _, err := LoadCatalog(writeCatalog(t, dir, raw)); err == nil {
			t.Fatal("invalid timeout accepted", bad)
		}
	}
}
