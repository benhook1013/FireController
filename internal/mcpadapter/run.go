// Package mcpadapter exposes bound read-only tools over the official MCP SDK.
package mcpadapter

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/benhook1013/FireController/internal/profile"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const Version = "0.1.0-dev"

type Diagnostic struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Action    string `json:"action"`
	Details   any    `json:"details,omitempty"`
	RequestID string `json:"request_id,omitempty"`
}

func (d *Diagnostic) Error() string { return d.Message }
func diagnostic(code, message, action string) *Diagnostic {
	return &Diagnostic{Code: code, Message: message, Action: action}
}

// New fixes one startup binding without reading credentials or contacting APIs.
func New(args []string, env map[string]string) (*mcp.Server, error) {
	name, path := "", ""
	seen := make(map[string]bool)
	for i := 0; i < len(args); i++ {
		flag, value, assigned := strings.Cut(args[i], "=")
		if (flag != "--profile" && flag != "--profiles-file") || seen[flag] {
			return nil, diagnostic("invalid_arguments", "Only one --profile and one --profiles-file option are supported.", "Choose the startup binding before connecting.")
		}
		seen[flag] = true
		if !assigned {
			i++
			if i >= len(args) || strings.HasPrefix(args[i], "-") {
				return nil, diagnostic("invalid_arguments", "A startup profile option requires a value.", "Provide --profile NAME or --profiles-file PATH.")
			}
			value = args[i]
		}
		if value == "" {
			return nil, diagnostic("invalid_arguments", "A startup profile option requires a value.", "Provide --profile NAME or --profiles-file PATH.")
		}
		if flag == "--profile" {
			name = value
		} else {
			path = value
		}
	}
	if !seen["--profile"] {
		name = env["FC_PROFILE"]
	}
	if name == "" || name != strings.TrimSpace(name) {
		return nil, diagnostic("profile_required", "A profile name is required without surrounding whitespace.", "Set --profile NAME or FC_PROFILE.")
	}
	if !seen["--profiles-file"] {
		directory := configDir(env)
		if directory == "" || !filepath.IsAbs(directory) {
			return nil, diagnostic("config_directory_unavailable", "The user configuration directory could not be determined.", "Provide --profiles-file PATH or configure the user environment.")
		}
		path = filepath.Join(directory, "firecontroller", "profiles.json")
	}
	binding, err := profile.LoadBinding(path, name)
	if err != nil {
		return nil, &Diagnostic{Code: err.Code, Message: err.Message, Action: err.Action}
	}
	return boundServer(binding), nil
}

func configDir(env map[string]string) string {
	switch runtime.GOOS {
	case "windows":
		for key, value := range env {
			if strings.EqualFold(key, "AppData") {
				return value
			}
		}
	case "darwin", "ios":
		if env["HOME"] != "" {
			return filepath.Join(env["HOME"], "Library", "Application Support")
		}
	case "plan9":
		if env["home"] != "" {
			return filepath.Join(env["home"], "lib")
		}
	default:
		if env["XDG_CONFIG_HOME"] != "" {
			if !filepath.IsAbs(env["XDG_CONFIG_HOME"]) {
				return ""
			}
			return env["XDG_CONFIG_HOME"]
		}
		if env["HOME"] != "" {
			return filepath.Join(env["HOME"], ".config")
		}
	}
	return ""
}

type writerCloser struct{ io.Writer }

func (writerCloser) Close() error { return nil }

// Run owns an SDK IO transport; callers should provide a closable stdin when
// they need cancellation to interrupt a blocked input read.
func Run(ctx context.Context, args []string, env map[string]string, stdin io.Reader, stdout, stderr io.Writer) int {
	server, err := New(args, env)
	if err != nil {
		var d *Diagnostic
		if !errors.As(err, &d) {
			d = diagnostic("configuration_invalid", "The startup binding is invalid.", "Correct the selected private profile.")
		}
		_ = json.NewEncoder(stderr).Encode(struct {
			Error *Diagnostic `json:"error"`
		}{d})
		if d.Code == "invalid_arguments" {
			return 2
		}
		return 3
	}
	reader, ok := stdin.(io.ReadCloser)
	if !ok {
		reader = io.NopCloser(stdin)
	}
	if err = server.Run(ctx, &mcp.IOTransport{Reader: reader, Writer: writerCloser{stdout}, MaxLineLength: 256 << 10}); err != nil {
		if ctx.Err() != nil {
			return 6
		}
		_ = json.NewEncoder(stderr).Encode(struct {
			Error *Diagnostic `json:"error"`
		}{diagnostic("protocol_failed", "The MCP connection could not be completed.", "Reconnect using a supported local stdio MCP client.")})
		return 5
	}
	return 0
}
