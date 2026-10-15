// Package cli provides the side-effect-free version and local context commands.
package cli

import (
	"encoding/json"
	"io"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/benhook1013/FireController/internal/profile"
)

const (
	ExitSuccess = 0
	ExitUsage   = 2
	ExitConfig  = 3
	Version     = "0.1.0-dev"
)

type diagnostic struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Action  string `json:"action"`
}

type options struct {
	command         string
	profile         string
	profilesFile    string
	hasProfile      bool
	hasProfilesFile bool
}

// Run reads only the provided environment map. It never changes process-global
// environment, selects a global current profile, or contacts a service.
func Run(args []string, env map[string]string, stdout, stderr io.Writer) int {
	opt, ok := parse(args)
	if !ok {
		return report(stderr, ExitUsage, "invalid_arguments", "Expected version or context and supported options.", "Use fc version or fc [--profile NAME] [--profiles-file PATH] context.")
	}
	if opt.command == "version" {
		if opt.hasProfile || opt.hasProfilesFile {
			return report(stderr, ExitUsage, "invalid_arguments", "The version command does not accept profile options.", "Use fc version.")
		}
		return emit(stdout, stderr, struct {
			Version string `json:"version"`
		}{Version})
	}
	if !opt.hasProfile {
		opt.profile = env["FC_PROFILE"]
	}
	if opt.profile == "" || opt.profile != strings.TrimSpace(opt.profile) {
		return report(stderr, ExitConfig, "profile_required", "A profile name is required without surrounding whitespace.", "Set --profile NAME or FC_PROFILE.")
	}
	if !opt.hasProfilesFile {
		directory := userConfigDir(env)
		if directory == "" || !filepath.IsAbs(directory) {
			return report(stderr, ExitConfig, "config_directory_unavailable", "The user configuration directory could not be determined.", "Provide --profiles-file PATH or configure the user environment.")
		}
		opt.profilesFile = filepath.Join(directory, "firecontroller", "profiles.json")
	}
	context, err := profile.Load(opt.profilesFile, opt.profile)
	if err != nil {
		return report(stderr, ExitConfig, err.Code, err.Message, err.Action)
	}
	return emit(stdout, stderr, context)
}

func parse(args []string) (options, bool) {
	var opt options
	for i := 0; i < len(args); i++ {
		argument := args[i]
		if !strings.HasPrefix(argument, "-") {
			if opt.command != "" || argument != "version" && argument != "context" {
				return opt, false
			}
			opt.command = argument
			continue
		}
		flag, value, assigned := strings.Cut(argument, "=")
		if flag != "--profile" && flag != "--profiles-file" {
			return opt, false
		}
		if !assigned {
			i++
			if i >= len(args) || strings.HasPrefix(args[i], "--") {
				return opt, false
			}
			value = args[i]
		}
		if value == "" {
			return opt, false
		}
		if flag == "--profile" {
			if opt.hasProfile {
				return opt, false
			}
			opt.profile, opt.hasProfile = value, true
		} else {
			if opt.hasProfilesFile {
				return opt, false
			}
			opt.profilesFile, opt.hasProfilesFile = value, true
		}
	}
	return opt, opt.command != ""
}

// This follows os.UserConfigDir's platform rules using the explicit process
// environment supplied to Run, so tests and embedded callers need no global
// environment mutation.
func userConfigDir(env map[string]string) string {
	switch runtime.GOOS {
	case "windows":
		if value, ok := env["APPDATA"]; ok {
			return value
		}
		// Embedded callers may provide conflicting case variants that cannot
		// coexist in an ordinary Windows process environment. Choose a stable
		// spelling rather than depending on map iteration order.
		selected := ""
		for key := range env {
			if strings.EqualFold(key, "APPDATA") && (selected == "" || key < selected) {
				selected = key
			}
		}
		return env[selected]
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

func emit(stdout, stderr io.Writer, value any) int {
	if err := json.NewEncoder(stdout).Encode(value); err != nil {
		return report(stderr, ExitConfig, "output_failed", "The response could not be written.", "Check the output destination before retrying.")
	}
	return ExitSuccess
}

func report(stderr io.Writer, exitCode int, code, message, action string) int {
	_ = json.NewEncoder(stderr).Encode(struct {
		Error diagnostic `json:"error"`
	}{diagnostic{Code: code, Message: message, Action: action}})
	return exitCode
}
