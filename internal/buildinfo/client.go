// Package buildinfo identifies the executable actually serving a connection.
package buildinfo

import (
	"encoding/hex"
	"runtime/debug"
)

type Client struct {
	Version        string `json:"version"`
	SourceRevision string `json:"source_revision"`
	SourceState    string `json:"source_state"`
}

// Connection retains string-valued local identity output for existing readers.
type Connection struct {
	ClientVersion        string `json:"client_version"`
	ClientSourceRevision string `json:"client_source_revision"`
	ClientSourceState    string `json:"client_source_state"`
}

func ConnectionInfo(version string) Connection {
	value := Current(version)
	return Connection{value.Version, value.SourceRevision, value.SourceState}
}

var revision, state = executableSource()

func Current(version string) Client {
	return Client{version, revision, state}
}

func executableSource() (string, string) {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown", "unknown"
	}
	return source(info.Settings)
}

func source(settings []debug.BuildSetting) (string, string) {
	revision, state := "unknown", "unknown"
	for _, setting := range settings {
		switch setting.Key {
		case "vcs.revision":
			if len(setting.Value) == 40 || len(setting.Value) == 64 {
				if _, err := hex.DecodeString(setting.Value); err == nil {
					revision = setting.Value
				}
			}
		case "vcs.modified":
			switch setting.Value {
			case "true":
				state = "modified"
			case "false":
				state = "clean"
			}
		}
	}
	if revision == "unknown" {
		state = "unknown"
	}
	return revision, state
}
