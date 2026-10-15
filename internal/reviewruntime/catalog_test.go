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
	path := writeCatalog(t, dir, `{"version":1,"capabilities":[{"id":"hosted","account":"account","organization":"example.org","channels":["hosted"],"github_credential_file":"github.token","git_executable":"git"}]}`)
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
	base := `{"version":1,"capabilities":[{"id":"first","account":"acct","organization":"example","channels":["hosted","cli"],"github_credential_file":"one.token","git_executable":"git","cli_executable":"coderabbit","authentication_environment":{"HOME":"/private/home"}}]}`
	firstPath := writeCatalog(t, dir, base)
	first, err := LoadCatalog(firstPath)
	if err != nil {
		t.Fatal(err)
	}
	if first.Capabilities[0].Channels[0] != "cli" || len(first.Capabilities[0].Fingerprint) != 64 || strings.ToLower(first.Capabilities[0].Fingerprint) != first.Capabilities[0].Fingerprint {
		t.Fatalf("channels/fingerprint are not canonical: %#v", first.Capabilities[0])
	}

	canonicalDifferentOrder := `{
  "capabilities": [{"authentication_environment":{"HOME":"/private/home"},"cli_executable":"coderabbit","git_executable":"git","github_credential_file":"one.token","channels":["cli","hosted"],"organization":"example","account":"acct","id":"first"}],
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
	otherRef := `{"version":1,"capabilities":[{"id":"first","account":"acct","organization":"example","channels":["hosted","cli"],"github_credential_file":` + quoteJSON(t, filepath.Join(dir, "one.token")) + `,"git_executable":"git","cli_executable":"coderabbit","authentication_environment":{"HOME":"/private/home"}}]}`
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
	return `{"version":1,"capabilities":[{"id":"hosted","account":"account","organization":"example","channels":["hosted"],"github_credential_file":` + quoteJSON(nil, reference) + `,"git_executable":"git"}]}`
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
