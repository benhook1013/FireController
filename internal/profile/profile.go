// Package profile validates private local configuration. It does not contact a
// service or verify the expected service identity.
package profile

import (
	"errors"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/benhook1013/FireController/internal/strictjson"
)

const MaxConfigBytes = 1 << 20

// Context contains only the selected local identity, never credentials.
type Context struct {
	Profile    string `json:"profile"`
	ServerURL  string `json:"server_url"`
	InstanceID string `json:"instance_id"`
	ProjectID  string `json:"project_id"`
	WorkerID   string `json:"worker_id"`
}

// Binding adds a private credential reference for service client construction.
type Binding struct {
	Context        Context `json:"context"`
	CredentialFile string  `json:"-"`
}

type entry struct {
	ServerURL      string `json:"server_url"`
	InstanceID     string `json:"instance_id"`
	ProjectID      string `json:"project_id"`
	WorkerID       string `json:"worker_id"`
	CredentialFile string `json:"credential_file"`
}

type configuration struct {
	Version  int              `json:"version"`
	Profiles map[string]entry `json:"profiles"`
}

// Error deliberately excludes file paths, configuration values, and parser
// excerpts. Its stable code and action can be safely printed by clients.
type Error struct {
	Code    string
	Message string
	Action  string
}

func (e *Error) Error() string { return e.Message }

func failure(code, message, action string) *Error {
	return &Error{Code: code, Message: message, Action: action}
}

// Load validates one explicitly named profile without reading its credential
// reference or modifying configuration. All entries must be valid so selection
// cannot hide malformed or ambiguous configuration.
func Load(path, name string) (Context, *Error) {
	binding, err := loadBinding(path, name, false)
	return binding.Context, err
}

// LoadBinding resolves the credential reference against the profile directory,
// without opening it or contacting the service.
func LoadBinding(path, name string) (Binding, *Error) { return loadBinding(path, name, true) }

func loadBinding(path, name string, resolve bool) (Binding, *Error) {
	var result Binding
	if !cleanValue(name) {
		return result, failure("profile_required", "A profile name is required without surrounding whitespace.", "Set --profile NAME or FC_PROFILE.")
	}
	// Check the path before opening so accidental FIFO/device inputs cannot
	// block the local command. This is not a hostile filesystem-race defense.
	info, err := os.Stat(path)
	if err != nil {
		return result, failure("profiles_unreadable", "The profiles configuration could not be inspected.", "Check --profiles-file and its read permissions.")
	}
	if !info.Mode().IsRegular() {
		return result, notRegularFile()
	}
	f, err := os.Open(path)
	if err != nil {
		return result, failure("profiles_unreadable", "The profiles configuration could not be opened.", "Check --profiles-file and its read permissions.")
	}
	defer f.Close()
	info, err = f.Stat()
	if err != nil {
		return result, failure("profiles_unreadable", "The opened profiles configuration could not be inspected.", "Check --profiles-file and its read permissions.")
	}
	if !info.Mode().IsRegular() {
		return result, notRegularFile()
	}
	data, err := io.ReadAll(io.LimitReader(f, MaxConfigBytes+1))
	if err != nil {
		return result, failure("profiles_unreadable", "The profiles configuration could not be read.", "Check --profiles-file and its read permissions.")
	}
	if len(data) > MaxConfigBytes {
		return result, failure("profiles_too_large", "The profiles configuration exceeds the 1 MiB limit.", "Reduce the configuration size.")
	}
	config, configErr := decodeConfiguration(data)
	if configErr != nil {
		return result, configErr
	}
	selected, ok := config.Profiles[name]
	if !ok {
		return result, failure("profile_not_found", "The selected profile does not exist.", "Select a configured name with --profile or FC_PROFILE.")
	}
	reference := ""
	if resolve {
		absolute, err := filepath.Abs(path)
		if err != nil {
			return result, failure("profiles_unreadable", "The profiles configuration location could not be resolved.", "Check --profiles-file and its location.")
		}
		reference = selected.CredentialFile
		if !filepath.IsAbs(reference) {
			reference = filepath.Join(filepath.Dir(absolute), reference)
		}
		reference = filepath.Clean(reference)
	}
	return Binding{Context: Context{Profile: name, ServerURL: selected.ServerURL, InstanceID: selected.InstanceID, ProjectID: selected.ProjectID, WorkerID: selected.WorkerID}, CredentialFile: reference}, nil
}

func decodeConfiguration(data []byte) (configuration, *Error) {
	var config configuration
	if err := strictjson.Decode(data, &config); err != nil {
		if errors.Is(err, strictjson.ErrEncoding) {
			return config, invalidEncoding()
		}
		return config, invalidJSON()
	}
	return config, validateConfiguration(config)
}

func validateConfiguration(config configuration) *Error {
	if config.Version != 1 {
		return failure("unsupported_profiles_version", "The profiles configuration must use version 1.", "Use the supported version 1 profiles schema.")
	}
	if len(config.Profiles) == 0 {
		return failure("profiles_invalid", "The profiles configuration must contain named profiles.", "Add a profile with the required service and identity fields.")
	}
	for name, candidate := range config.Profiles {
		// Check supplied strings before JSON marshaling can replace invalid bytes.
		for _, value := range []string{name, candidate.ServerURL, candidate.InstanceID, candidate.ProjectID, candidate.WorkerID, candidate.CredentialFile} {
			if !utf8.ValidString(value) {
				return invalidEncoding()
			}
		}
		if !cleanValue(name) || !cleanValue(candidate.InstanceID) || !cleanValue(candidate.ProjectID) || !cleanValue(candidate.WorkerID) || !cleanValue(candidate.CredentialFile) {
			return failure("profiles_invalid", "Each profile requires an unambiguous name, instance ID, project ID, worker ID, and credential reference.", "Provide nonempty fields without surrounding whitespace.")
		}
		if !safeURL(candidate.ServerURL) {
			return failure("profiles_unsafe_url", "Each server URL must use HTTP or HTTPS without user information, a query, or a fragment; remote hosts require HTTPS.", "Correct the server URL in the profiles configuration.")
		}
	}
	return nil
}

func notRegularFile() *Error {
	return failure("profiles_not_regular", "The profiles configuration must be a regular file.", "Select a regular configuration file with --profiles-file.")
}

func invalidJSON() *Error {
	return failure("profiles_invalid_json", "The profiles configuration must be one strict JSON object with known fields and unique keys.", "Correct malformed, trailing, duplicate, or unknown fields in the version 1 configuration.")
}

func invalidEncoding() *Error {
	return failure("profiles_invalid_encoding", "The profiles configuration must contain valid UTF-8 bytes and Unicode escapes.", "Save valid UTF-8 and pair each high surrogate escape with a low surrogate escape.")
}

func cleanValue(value string) bool {
	return value != "" && value == strings.TrimSpace(value)
}

func safeURL(value string) bool {
	if !cleanValue(value) || strings.Contains(value, "#") {
		return false
	}
	u, err := url.Parse(value)
	if err != nil || u.Opaque != "" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return false
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return false
	}
	if u.Scheme == "https" {
		return true
	}
	host := u.Hostname()
	return strings.EqualFold(host, "localhost") || net.ParseIP(host).IsLoopback()
}
