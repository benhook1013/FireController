package offsite

import (
	"os"
	"path/filepath"
	"runtime"
)

// InstallationDependency is private operator inventory, not public telemetry.
// Path may identify private configuration. No credential contents are read.
// State is a filesystem observation, never executable/credential qualification.
type InstallationDependency struct {
	Role     string `json:"role"`
	Provider int    `json:"provider"` // -1 denotes a shared dependency.
	Path     string `json:"path"`
	State    string `json:"state"`
}

type BatchInstallationInspection struct {
	Dependencies      []InstallationDependency `json:"dependencies"`
	InstallationReady bool                     `json:"installation_ready"` // Always false: closure is incomplete.
}

// InspectBatchInstallation reuses the runtime's canonical batch/provider parser
// and checks direct filesystem dependencies without executing tools, opening
// credential contents, creating state/locks, or contacting providers. Run in the
// target mount namespace. No lease or immutable-file custody is acquired; callers
// must revalidate under installation custody before switching. INI references,
// service units/environment, interpreters/libraries, provenance, free space and
// actual offline compatibility remain separate requirements.
func InspectBatchInstallation(path string) (BatchInstallationInspection, error) {
	b, configs, err := LoadBatchConfiguration(path)
	if err != nil {
		return BatchInstallationInspection{}, err
	}
	var out BatchInstallationInspection
	add := func(role string, provider int, path, kind string, private bool) {
		out.Dependencies = append(out.Dependencies, InstallationDependency{role, provider, path, installationPathState(path, kind, private)})
	}
	add("batch_configuration", -1, path, "file", true)
	add("batch_state", -1, b.StateDir, "directory", true)
	add("source", -1, configs[0].SourceRoot, "directory", true)
	if b.PhaseLease != "" {
		add("phase_lease", -1, b.PhaseLease, "file", true)
	}
	for i, c := range configs {
		add("provider_configuration", i, b.Configs[i], "file", true)
		add("provider_state", i, c.StateDir, "directory", true)
		add("transport_executable", i, c.RcloneBinary, "executable", false)
		add("transport_configuration", i, c.RcloneConfig, "file", true)
	}
	return out, nil
}

func installationPathState(path, kind string, private bool) string {
	// Refuse aliases in every component rather than inspecting a different target
	// from the one the installation manifest appears to name.
	for p := path; ; p = filepath.Dir(p) {
		info, err := os.Lstat(p)
		if err != nil {
			if os.IsNotExist(err) {
				return "missing"
			}
			return "unavailable"
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return "symlink"
		}
		if p != path && !info.IsDir() {
			return "wrong_type"
		}
		if filepath.Dir(p) == p {
			break
		}
	}
	info, err := os.Lstat(path)
	if err != nil {
		return "unavailable"
	}
	if kind == "directory" {
		if !info.IsDir() {
			return "wrong_type"
		}
	} else if !info.Mode().IsRegular() {
		return "wrong_type"
	}
	if runtime.GOOS != "windows" {
		if private && info.Mode().Perm()&0077 != 0 {
			return "not_private"
		}
		if kind == "executable" && info.Mode().Perm()&0111 == 0 {
			return "not_executable"
		}
	}
	return "present"
}
