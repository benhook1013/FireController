package offsite

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func preflightConfigFixture(t *testing.T) (string, BatchConfig, Config) {
	t.Helper()
	root := t.TempDir()
	path := filepath.Join(root, "batch.json")
	b := BatchConfig{Version: 1, StateDir: filepath.Join(root, "batch-state"), Configs: []string{filepath.Join(root, "provider.json")}, MinFreeBytes: 1}
	c := Config{Version: 1, SourceRoot: filepath.Join(root, "source"), InstanceID: "instance_" + strings.Repeat("a", 32), StateDir: filepath.Join(root, "provider-state"), RcloneBinary: filepath.Join(root, "rclone"), RcloneConfig: filepath.Join(root, "credentials"), Remote: "remote:backups", Keep: 24, Timeout: "1h", MaxBundleBytes: 1 << 20}
	savePreflightJSON(t, path, b)
	savePreflightJSON(t, b.Configs[0], c)
	return path, b, c
}

func savePreflightJSON(t *testing.T, path string, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestLoadBatchConfigurationReadOnlyAndExplicitLimits(t *testing.T) {
	path, b, c := preflightConfigFixture(t)
	before, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	got, cs, err := LoadBatchConfiguration(path)
	if err != nil || got.StateDir != b.StateDir || len(cs) != 1 || cs[0] != c {
		t.Fatalf("unexpected configuration: %v", err)
	}
	// Source, state, transport and credential files deliberately do not exist.
	// Successful parsing must neither create them nor claim their readiness.
	after, err := os.ReadDir(filepath.Dir(path))
	if err != nil || len(after) != len(before) {
		t.Fatal("inspection wrote artifacts", err)
	}
	for _, p := range []string{b.StateDir, c.StateDir, c.SourceRoot, c.RcloneBinary, c.RcloneConfig} {
		if _, err := os.Lstat(p); !os.IsNotExist(err) {
			t.Fatal("inspection created dependency", err)
		}
	}
}

func TestLoadBatchConfigurationRejectsTransitiveInvalidity(t *testing.T) {
	for _, variant := range []string{"provider-unknown", "provider-duplicate", "missing-provider", "batch-unknown", "credential-overlap", "lease-overlap", "source-mismatch"} {
		t.Run(variant, func(t *testing.T) {
			path, b, c := preflightConfigFixture(t)
			switch variant {
			case "provider-unknown", "provider-duplicate":
				raw, _ := os.ReadFile(b.Configs[0])
				key := `"unexpected":true,`
				if variant == "provider-duplicate" {
					key = `"version":1,`
				}
				if err := os.WriteFile(b.Configs[0], append([]byte("{"+key), raw[1:]...), 0600); err != nil {
					t.Fatal(err)
				}
			case "missing-provider":
				b.Configs[0] += ".missing"
				savePreflightJSON(t, path, b)
			case "batch-unknown":
				raw, _ := os.ReadFile(path)
				if err := os.WriteFile(path, append([]byte(`{"unexpected":true,`), raw[1:]...), 0600); err != nil {
					t.Fatal(err)
				}
			case "credential-overlap":
				c.RcloneConfig = filepath.Join(b.StateDir, "credential")
				savePreflightJSON(t, b.Configs[0], c)
			case "lease-overlap":
				b.PhaseLease = filepath.Join(c.SourceRoot, "phase.lock")
				savePreflightJSON(t, path, b)
			case "source-mismatch":
				other := c
				other.SourceRoot += "-other"
				other.StateDir += "-other"
				other.Remote = "remote:other"
				b.Configs = append(b.Configs, filepath.Join(filepath.Dir(path), "other.json"))
				savePreflightJSON(t, b.Configs[1], other)
				savePreflightJSON(t, path, b)
			}
			if _, _, err := LoadBatchConfiguration(path); err == nil {
				t.Fatal("invalid dependency graph accepted")
			}
			if _, err := os.Lstat(b.StateDir); !os.IsNotExist(err) {
				t.Fatal("inspection changed state", err)
			}
		})
	}
}

// Runtime retains parse errors before cancellation, and cancellation before
// cross-provider validation. Inspection has no context and validates the graph.
func TestBatchConfigurationCancellationPrecedence(t *testing.T) {
	for _, nilContext := range []bool{false, true} {
		for _, variant := range []string{"credential-overlap", "lease-overlap", "malformed-provider", "malformed-batch", "relative-path"} {
			t.Run(fmt.Sprintf("%t/%s", nilContext, variant), func(t *testing.T) {
				path, b, c := preflightConfigFixture(t)
				want := "offsite_cancelled"
				switch variant {
				case "credential-overlap":
					c.RcloneConfig = filepath.Join(b.StateDir, "credential")
					savePreflightJSON(t, b.Configs[0], c)
				case "lease-overlap":
					b.PhaseLease = filepath.Join(c.SourceRoot, "phase.lock")
					savePreflightJSON(t, path, b)
				case "malformed-provider":
					if err := os.WriteFile(b.Configs[0], []byte("{"), 0600); err != nil {
						t.Fatal(err)
					}
					want = "offsite_configuration_invalid"
				case "malformed-batch":
					if err := os.WriteFile(path, []byte("{"), 0600); err != nil {
						t.Fatal(err)
					}
					want = "offsite_configuration_invalid"
				case "relative-path":
					path = "relative.json"
					want = "offsite_configuration_invalid"
				}
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				if nilContext {
					ctx = nil
				}
				_, err := PushBatch(ctx, path)
				wantCode(t, err, want)
				if _, err := os.Lstat(b.StateDir); !os.IsNotExist(err) {
					t.Fatal("runtime changed state", err)
				}
				_, _, err = LoadBatchConfiguration(path)
				wantCode(t, err, "offsite_configuration_invalid")
			})
		}
	}
}
