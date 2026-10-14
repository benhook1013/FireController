package profile

import (
	"maps"
	"path/filepath"
	"testing"
)

func TestConfigDirectoryUsesExplicitDeterministicEnvironment(t *testing.T) {
	root := t.TempDir()
	for _, tc := range []struct {
		name, goos string
		env        map[string]string
		want       string
	}{
		{"windows exact", "windows", map[string]string{"APPDATA": "exact", "AppData": "mixed", "appdata": "lower"}, "exact"},
		{"windows variants", "windows", map[string]string{"AppData": "mixed", "appdata": "lower"}, "mixed"},
		{"windows explicit empty", "windows", map[string]string{"APPDATA": "", "AppData": "mixed"}, ""},
		{"windows absent", "windows", map[string]string{"HOME": root}, ""},
		{"linux XDG", "linux", map[string]string{"XDG_CONFIG_HOME": root, "HOME": "ignored"}, root},
		{"linux relative XDG", "linux", map[string]string{"XDG_CONFIG_HOME": "relative", "HOME": root}, ""},
		{"linux HOME", "linux", map[string]string{"HOME": root}, filepath.Join(root, ".config")},
		{"macOS", "darwin", map[string]string{"HOME": root}, filepath.Join(root, "Library", "Application Support")},
		{"plan9", "plan9", map[string]string{"home": root}, filepath.Join(root, "lib")},
		{"missing", "linux", map[string]string{}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			original := maps.Clone(tc.env)
			for range 100 {
				if got := configDirectory(tc.goos, tc.env); got != tc.want {
					t.Fatalf("directory=%q want%q", got, tc.want)
				}
			}
			if !maps.Equal(original, tc.env) {
				t.Fatal("resolution changed caller environment")
			}
		})
	}
}
