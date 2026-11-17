package profile

import (
	"path/filepath"
	"runtime"
	"strings"
)

// ConfigDirectory follows the current platform's user-configuration rules using
// the caller's explicit environment, without reading or changing process state.
func ConfigDirectory(env map[string]string) string {
	return configDirectory(runtime.GOOS, env)
}

func configDirectory(goos string, env map[string]string) string {
	switch goos {
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
