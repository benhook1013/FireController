// Package reviewruntime loads private operator-selected local review capabilities.
package reviewruntime

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/strictjson"
)

const maxCatalogBytes = 64 << 10
const maxCredentialBytes = 4096
const maxCredentialTokenBytes = 256

// Catalog is a loaded private operator configuration. Its String method and
// JSON representation omit all paths and credentials.
type Catalog struct {
	Version      int                  `json:"version"`
	Capabilities []OperatorCapability `json:"capabilities"`
}

func (c Catalog) String() string {
	return fmt.Sprintf("Catalog{version:%d capabilities:%d}", c.Version, len(c.Capabilities))
}

func (c Catalog) GoString() string { return c.String() }

// OperatorCapability describes one operator-owned local adapter capability.
// The credential is accessible only through GitHubCredential; private paths are
// omitted when this value is logged or marshaled.
type OperatorCapability struct {
	ID                        string            `json:"id"`
	Account                   string            `json:"account"`
	Organization              string            `json:"organization"`
	Channels                  []string          `json:"channels"`
	GitExecutable             string            `json:"-"`
	CLIExecutable             string            `json:"-"`
	AuthenticationEnvironment map[string]string `json:"-"`
	Fingerprint               string            `json:"fingerprint"`
	githubCredential          string
}

func (c OperatorCapability) String() string           { return fmt.Sprintf("OperatorCapability{id:%q}", c.ID) }
func (c OperatorCapability) GoString() string         { return c.String() }
func (c OperatorCapability) GitHubCredential() string { return c.githubCredential }

type catalogFile struct {
	Version      int              `json:"version"`
	Capabilities []capabilityFile `json:"capabilities"`
}
type capabilityFile struct {
	ID                        string            `json:"id"`
	Account                   string            `json:"account"`
	Organization              string            `json:"organization"`
	Channels                  []string          `json:"channels"`
	GitHubCredentialFile      string            `json:"github_credential_file"`
	GitExecutable             string            `json:"git_executable"`
	CLIExecutable             string            `json:"cli_executable"`
	AuthenticationEnvironment map[string]string `json:"authentication_environment"`
}

var allowedAuthenticationEnvironment = map[string]bool{
	"HOME": true, "USERPROFILE": true, "APPDATA": true, "LOCALAPPDATA": true,
	"XDG_CONFIG_HOME": true, "XDG_DATA_HOME": true, "XDG_CACHE_HOME": true,
}

// LoadCatalog reads one explicitly selected private catalog. An empty path
// disables native execution; it never implies a file in the current directory.
func LoadCatalog(path string) (Catalog, error) {
	if path == "" {
		return Catalog{Version: 1, Capabilities: make([]OperatorCapability, 0)}, nil
	}
	data, absolute, err := readPrivateFile(path, maxCatalogBytes)
	if err != nil {
		return Catalog{}, configurationError()
	}
	if strictjson.Validate(data) != nil {
		return Catalog{}, configurationError()
	}
	var file catalogFile
	if strictjson.Decode(data, &file) != nil || file.Version != 1 || file.Capabilities == nil || len(file.Capabilities) > 32 {
		return Catalog{}, configurationError()
	}
	if !requiredNonNullObject(data, "version", "capabilities") {
		return Catalog{}, configurationError()
	}
	if err := validateCapabilityShapes(data, file.Capabilities); err != nil {
		return Catalog{}, configurationError()
	}
	result := Catalog{Version: 1, Capabilities: make([]OperatorCapability, 0, len(file.Capabilities))}
	ids := make(map[string]bool, len(file.Capabilities))
	for _, input := range file.Capabilities {
		capability, err := loadCapability(input, filepath.Dir(absolute))
		if err != nil || ids[capability.ID] {
			return Catalog{}, configurationError()
		}
		ids[capability.ID] = true
		result.Capabilities = append(result.Capabilities, capability)
	}
	return result, nil
}

func validateCapabilityShapes(data []byte, capabilities []capabilityFile) error {
	var root map[string]json.RawMessage
	if json.Unmarshal(data, &root) != nil || root == nil {
		return errors.New("shape")
	}
	var rawCaps []json.RawMessage
	if json.Unmarshal(root["capabilities"], &rawCaps) != nil || len(rawCaps) != len(capabilities) {
		return errors.New("shape")
	}
	for _, raw := range rawCaps {
		var fields map[string]json.RawMessage
		if json.Unmarshal(raw, &fields) != nil || fields == nil {
			return errors.New("shape")
		}
		for _, name := range []string{"id", "account", "organization", "channels", "git_executable"} {
			if !nonNull(fields, name) {
				return errors.New("shape")
			}
		}
		for _, name := range []string{"github_credential_file", "cli_executable", "authentication_environment"} {
			if value, ok := fields[name]; ok && isNull(value) {
				return errors.New("shape")
			}
		}
		if rawEnv, ok := fields["authentication_environment"]; ok {
			var environment map[string]json.RawMessage
			if json.Unmarshal(rawEnv, &environment) != nil || environment == nil {
				return errors.New("shape")
			}
			for _, value := range environment {
				if isNull(value) {
					return errors.New("shape")
				}
			}
		}
	}
	return nil
}

func loadCapability(input capabilityFile, catalogDirectory string) (OperatorCapability, error) {
	var empty OperatorCapability
	if !validIdentifier(input.ID) || !validIdentifier(input.Account) || !validOrganization(input.Organization) || len(input.Channels) < 1 || len(input.Channels) > 2 || !validExecutable(input.GitExecutable) || len(input.AuthenticationEnvironment) > 7 {
		return empty, configurationError()
	}
	channels := append([]string(nil), input.Channels...)
	seen := make(map[string]bool, len(channels))
	for _, channel := range channels {
		if channel != "cli" && channel != "hosted" || seen[channel] {
			return empty, configurationError()
		}
		seen[channel] = true
	}
	sort.Strings(channels)
	if seen["cli"] && !validExecutable(input.CLIExecutable) || !seen["cli"] && input.CLIExecutable != "" && !validExecutable(input.CLIExecutable) {
		return empty, configurationError()
	}
	environment := make(map[string]string, len(input.AuthenticationEnvironment))
	for key, value := range input.AuthenticationEnvironment {
		if !allowedAuthenticationEnvironment[key] || !validAbsolutePath(value) {
			return empty, configurationError()
		}
		environment[key] = value
	}
	token := ""
	if input.GitHubCredentialFile != "" {
		credentialReference := input.GitHubCredentialFile
		if !filepath.IsAbs(credentialReference) {
			credentialReference = filepath.Join(catalogDirectory, credentialReference)
		}
		credentialReference = filepath.Clean(credentialReference)
		var err error
		token, err = readCredential(credentialReference)
		if err != nil || token == "" {
			return empty, configurationError()
		}
	} else if seen["hosted"] {
		return empty, configurationError()
	}
	if len(token) > maxCredentialTokenBytes || containsWhitespaceOrControl(token) {
		return empty, configurationError()
	}
	capability := OperatorCapability{ID: input.ID, Account: input.Account, Organization: input.Organization, Channels: channels, GitExecutable: input.GitExecutable, CLIExecutable: input.CLIExecutable, AuthenticationEnvironment: environment, githubCredential: token}
	fingerprint, err := capabilityFingerprint(capability)
	if err != nil {
		return empty, configurationError()
	}
	capability.Fingerprint = fingerprint
	return capability, nil
}

func capabilityFingerprint(capability OperatorCapability) (string, error) {
	tokenHash := sha256.Sum256([]byte(capability.githubCredential))
	canonical := struct {
		ID                        string            `json:"id"`
		Account                   string            `json:"account"`
		Organization              string            `json:"organization"`
		Channels                  []string          `json:"channels"`
		GitExecutable             string            `json:"git_executable"`
		CLIExecutable             string            `json:"cli_executable"`
		AuthenticationEnvironment map[string]string `json:"authentication_environment"`
		CredentialSHA256          string            `json:"credential_sha256"`
	}{capability.ID, capability.Account, capability.Organization, capability.Channels, capability.GitExecutable, capability.CLIExecutable, capability.AuthenticationEnvironment, hex.EncodeToString(tokenHash[:])}
	data, err := json.Marshal(canonical)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}

func readPrivateFile(path string, limit int64) ([]byte, string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, "", err
	}
	before, err := os.Lstat(absolute)
	if err != nil || !privateRegular(before) {
		return nil, "", errors.New("file")
	}
	if before.Size() > limit {
		return nil, "", errors.New("large")
	}
	file, err := os.Open(absolute)
	if err != nil {
		return nil, "", err
	}
	defer file.Close()
	after, err := file.Stat()
	if err != nil || !privateRegular(after) || !os.SameFile(before, after) {
		return nil, "", errors.New("file")
	}
	pathAfter, err := os.Lstat(absolute)
	if err != nil || !privateRegular(pathAfter) || !os.SameFile(before, pathAfter) {
		return nil, "", errors.New("file")
	}
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil || int64(len(data)) > limit {
		return nil, "", errors.New("large")
	}
	afterRead, err := file.Stat()
	if err != nil || !privateRegular(afterRead) || !os.SameFile(before, afterRead) || afterRead.Size() > limit {
		return nil, "", errors.New("file")
	}
	return data, absolute, nil
}

func readCredential(path string) (string, error) {
	data, _, err := readPrivateFile(path, maxCredentialBytes)
	if err != nil || !utf8.Valid(data) {
		return "", errors.New("credential")
	}
	token := strings.TrimSpace(string(data))
	if token == "" || len(token) > maxCredentialTokenBytes || containsWhitespaceOrControl(token) {
		return "", errors.New("credential")
	}
	return token, nil
}

func privateRegular(info os.FileInfo) bool {
	return info != nil && info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0 && (runtime.GOOS == "windows" || info.Mode().Perm()&0o077 == 0)
}
func validIdentifier(value string) bool   { return asciiName(value, 128, "_-", false) }
func validOrganization(value string) bool { return asciiName(value, 100, "._-", true) }
func asciiName(value string, max int, extra string, dotOnlyInvalid bool) bool {
	if len(value) < 1 || len(value) > max || !utf8.ValidString(value) {
		return false
	}
	onlyDots := true
	for _, b := range []byte(value) {
		if b != '.' {
			onlyDots = false
		}
		if !(b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' || strings.ContainsRune(extra, rune(b))) {
			return false
		}
	}
	return !dotOnlyInvalid || !onlyDots
}
func validExecutable(value string) bool {
	return value != "" && len(value) <= 4096 && utf8.ValidString(value) && !strings.ContainsRune(value, 0)
}
func validAbsolutePath(value string) bool {
	return value != "" && len(value) <= 4096 && utf8.ValidString(value) && !strings.ContainsRune(value, 0) && filepath.IsAbs(value)
}
func containsWhitespaceOrControl(value string) bool {
	for _, r := range value {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return true
		}
	}
	return false
}

func requiredNonNullObject(data []byte, names ...string) bool {
	var fields map[string]json.RawMessage
	if json.Unmarshal(data, &fields) != nil || fields == nil {
		return false
	}
	for _, name := range names {
		if !nonNull(fields, name) {
			return false
		}
	}
	return true
}
func nonNull(fields map[string]json.RawMessage, name string) bool {
	value, ok := fields[name]
	return ok && !isNull(value)
}
func isNull(raw json.RawMessage) bool { return strings.TrimSpace(string(raw)) == "null" }

func configurationError() error {
	return core.Fail("configuration_invalid", "The private review runtime catalog is invalid or unavailable.", 500)
}
