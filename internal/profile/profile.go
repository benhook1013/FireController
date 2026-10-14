// Package profile validates private local configuration. It does not contact a
// service or verify the expected service identity.
package profile

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/url"
	"os"
	"strings"
	"unicode/utf8"
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
	var result Context
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
	if !utf8.Valid(data) {
		return result, failure("profiles_invalid_encoding", "The profiles configuration must contain valid UTF-8 bytes.", "Save the configuration as UTF-8 without malformed byte sequences.")
	}
	// The normal JSON decoder permits duplicate keys; reject them first because
	// a repeated profile or identity must never silently replace another value.
	decoder := json.NewDecoder(bytes.NewReader(data))
	if !uniqueValue(decoder, 0) {
		return result, invalidJSON()
	}
	if _, err := decoder.Token(); err != io.EOF {
		return result, invalidJSON()
	}
	if !exactFields(data) {
		return result, invalidJSON()
	}
	var config configuration
	decoder = json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&config); err != nil {
		return result, invalidJSON()
	}
	if config.Version != 1 {
		return result, failure("unsupported_profiles_version", "The profiles configuration must use version 1.", "Use the supported version 1 profiles schema.")
	}
	if len(config.Profiles) == 0 {
		return result, failure("profiles_invalid", "The profiles configuration must contain named profiles.", "Add a profile with the required service and identity fields.")
	}
	for profileName, candidate := range config.Profiles {
		if !cleanValue(profileName) || !cleanValue(candidate.InstanceID) || !cleanValue(candidate.ProjectID) || !cleanValue(candidate.WorkerID) || !cleanValue(candidate.CredentialFile) {
			return result, failure("profiles_invalid", "Each profile requires an unambiguous name, instance ID, project ID, worker ID, and credential reference.", "Provide nonempty fields without surrounding whitespace.")
		}
		if !safeURL(candidate.ServerURL) {
			return result, failure("profiles_unsafe_url", "Each server URL must use HTTP or HTTPS without user information, a query, or a fragment; remote hosts require HTTPS.", "Correct the server URL in the profiles configuration.")
		}
	}
	selected, ok := config.Profiles[name]
	if !ok {
		return result, failure("profile_not_found", "The selected profile does not exist.", "Select a configured name with --profile or FC_PROFILE.")
	}
	return Context{Profile: name, ServerURL: selected.ServerURL, InstanceID: selected.InstanceID, ProjectID: selected.ProjectID, WorkerID: selected.WorkerID}, nil
}

func notRegularFile() *Error {
	return failure("profiles_not_regular", "The profiles configuration must be a regular file.", "Select a regular configuration file with --profiles-file.")
}

func invalidJSON() *Error {
	return failure("profiles_invalid_json", "The profiles configuration must be one strict JSON object with known fields and unique keys.", "Correct malformed, trailing, duplicate, or unknown fields in the version 1 configuration.")
}

// encoding/json also matches struct fields without regard to case. Enforce the
// documented keys exactly so differently cased identity keys cannot override
// one another or act as undocumented aliases.
func exactFields(data []byte) bool {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(data, &root); err != nil || root == nil {
		return false
	}
	for key := range root {
		if key != "version" && key != "profiles" {
			return false
		}
	}
	if raw, present := root["profiles"]; present {
		var profiles map[string]json.RawMessage
		if err := json.Unmarshal(raw, &profiles); err != nil {
			return false
		}
		for _, rawEntry := range profiles {
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(rawEntry, &fields); err != nil {
				return false
			}
			for key := range fields {
				switch key {
				case "server_url", "instance_id", "project_id", "worker_id", "credential_file":
				default:
					return false
				}
			}
		}
	}
	return true
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

func uniqueValue(decoder *json.Decoder, depth int) bool {
	if depth > 64 {
		return false
	}
	token, err := decoder.Token()
	if err != nil {
		return false
	}
	delimiter, nested := token.(json.Delim)
	if !nested {
		return true
	}
	switch delimiter {
	case '{':
		keys := make(map[string]bool)
		for decoder.More() {
			key, err := decoder.Token()
			if err != nil {
				return false
			}
			name, ok := key.(string)
			if !ok || keys[name] || !uniqueValue(decoder, depth+1) {
				return false
			}
			keys[name] = true
		}
	case '[':
		for decoder.More() {
			if !uniqueValue(decoder, depth+1) {
				return false
			}
		}
	default:
		return false
	}
	end, err := decoder.Token()
	return err == nil && (delimiter == '{' && end == json.Delim('}') || delimiter == '[' && end == json.Delim(']'))
}
