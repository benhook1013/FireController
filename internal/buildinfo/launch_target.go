package buildinfo

import (
	fileinfo "debug/buildinfo"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
)

// Source identifies local executable bytes, not an update service or authority.
type Source struct {
	Revision string
	State    string
}

// LaunchTarget reads the current target of the absolute invocation path. A
// managed current/bin launcher can therefore reveal an updated installation
// while Current still identifies the older executable actually running.
func LaunchTarget() (Source, bool) {
	return launchTarget(os.Args[0], fileinfo.ReadFile)
}

func launchTarget(path string, read func(string) (*debug.BuildInfo, error)) (Source, bool) {
	name := strings.TrimSuffix(filepath.Base(path), ".exe")
	if !filepath.IsAbs(path) || name != "fc" && name != "fcmcp" {
		return Source{}, false
	}
	// Refuse devices, pipes and unbounded resolved files before the metadata
	// reader opens them. Either the binary or its directory may be a stable link.
	stat, err := os.Stat(path)
	if err != nil || !stat.Mode().IsRegular() || stat.Size() < 1 || stat.Size() > 256<<20 || runtime.GOOS != "windows" && stat.Mode().Perm()&0111 == 0 {
		return Source{}, false
	}
	info, err := read(path)
	if err != nil || info == nil || info.Path != "github.com/benhook1013/FireController/cmd/"+name {
		return Source{}, false
	}
	revision, state := source(info.Settings)
	if revision == "unknown" {
		return Source{}, false
	}
	return Source{Revision: revision, State: state}, true
}
