package offsite

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/benhook1013/FireController/internal/backup"
	"github.com/benhook1013/FireController/internal/backupphase"
	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/strictjson"
)

// BatchConfig explicitly opts a finite set of destinations into serial work.
// Destination configurations and their durable ownership ledgers stay separate.
type BatchConfig struct {
	PhaseLease   string   `json:"phase_lease,omitempty"`
	Version      int      `json:"version"`
	StateDir     string   `json:"state_dir"`
	Configs      []string `json:"configs"`
	MinFreeBytes int64    `json:"min_free_bytes"`
	configPath   string
}
type BatchDestinationResult struct {
	Index  int    `json:"index"`
	Result Result `json:"result"`
	Reason string `json:"reason,omitempty"`
}
type BatchResult struct {
	Destinations []BatchDestinationResult `json:"destinations"`
}

type batchPreparation struct {
	work     string
	selected *backup.Summary
	receipt  *Receipt
	floor    int64
	verified map[Receipt]bool
}

func (p *batchPreparation) selectSource(ctx context.Context, c Config) (backup.Summary, error) {
	if p.selected != nil {
		return *p.selected, nil
	}
	s, err := newest(ctx, c)
	if err != nil {
		return s, err
	}
	p.selected = &s
	return s, nil
}
func (p *batchPreparation) linkArchive(ctx context.Context, s backup.Summary, target string, c Config) (Receipt, error) {
	archive := filepath.Join(p.work, "shared.tar.gz")
	if p.receipt == nil {
		if err := p.admit(p.work, c.MaxBundleBytes); err != nil {
			return Receipt{}, err
		}
		r, err := packVerified(ctx, s, archive, c)
		if err != nil {
			return r, err
		}
		stage, err := os.MkdirTemp(p.work, "inspect-")
		if err != nil {
			return Receipt{}, failure("offsite_storage_unavailable")
		}
		err = p.validateArchive(ctx, archive, stage, r, c)
		cleanup := os.RemoveAll(stage)
		if err != nil {
			os.Remove(archive)
			return Receipt{}, err
		}
		if cleanup != nil {
			return Receipt{}, failure("offsite_storage_unavailable")
		}
		p.receipt = &r
	}
	if ctx.Err() != nil {
		return Receipt{}, failure("offsite_cancelled")
	}
	// A hard link is the destination's durable reference. Renaming/removing one
	// provider's spool never removes another provider's pending bytes. Fail closed
	// across filesystems rather than silently multiplying archive storage.
	if os.Link(archive, target) != nil || syncDir(filepath.Dir(target)) != nil {
		return Receipt{}, failure("offsite_storage_unavailable")
	}
	return *p.receipt, nil
}
func pathsOverlap(a, b string) bool {
	a, b = resolvePrefix(a), resolvePrefix(b)
	return a == b || strings.HasPrefix(a, b+string(filepath.Separator)) || strings.HasPrefix(b, a+string(filepath.Separator))
}

// LoadBatchConfiguration reads the batch and its provider configurations using
// the same validation as PushBatch, without creating directories, acquiring
// locks, reading credential contents, executing transports or contacting remotes.
// Returned paths describe dependencies, not verified executable provenance or
// recovery compatibility. Callers must not treat this as installation readiness.
func LoadBatchConfiguration(path string) (BatchConfig, []Config, error) {
	b, configs, err := loadBatchConfiguration(path)
	if err != nil {
		return BatchConfig{}, nil, err
	}
	if err := validateBatchConfiguration(b, configs); err != nil {
		return BatchConfig{}, nil, err
	}
	return b, configs, nil
}

// loadBatchConfiguration preserves runtime parsing before context admission;
// cross-configuration validation follows the context check in pushBatch.
func loadBatchConfiguration(path string) (BatchConfig, []Config, error) {
	var b BatchConfig
	if !absolute(path) {
		return BatchConfig{}, nil, failure("offsite_configuration_invalid")
	}
	raw, err := privateRead(path, 16<<10)
	if err != nil {
		return BatchConfig{}, nil, err
	}
	if strictjson.Decode(raw, &b) != nil || b.Version != 1 || !absolute(b.StateDir) || len(b.Configs) < 1 || len(b.Configs) > 8 || b.MinFreeBytes <= 0 || b.MinFreeBytes > maxBundleBytes || pathsOverlap(b.StateDir, path) {
		return BatchConfig{}, nil, failure("offsite_configuration_invalid")
	}
	b.configPath = path
	configs := make([]Config, len(b.Configs))
	for i, path := range b.Configs {
		if !absolute(path) {
			return BatchConfig{}, nil, failure("offsite_configuration_invalid")
		}
		c, e := loadConfig(path)
		if e != nil {
			return BatchConfig{}, nil, e
		}
		configs[i] = c
	}
	return b, configs, nil
}
func PushBatch(ctx context.Context, path string) (BatchResult, error) {
	b, configs, err := loadBatchConfiguration(path)
	if err != nil {
		return BatchResult{}, err
	}
	return pushBatch(ctx, b, configs, nil)
}

// validateBatchConfiguration is shared by read-only inspection and execution.
func validateBatchConfiguration(b BatchConfig, configs []Config) error {
	if len(configs) == 0 || len(configs) > 8 || len(configs) != len(b.Configs) || !absolute(b.StateDir) || b.MinFreeBytes <= 0 || b.MinFreeBytes > maxBundleBytes {
		return failure("offsite_configuration_invalid")
	}
	for i, c := range configs {
		if c.SourceRoot != configs[0].SourceRoot || c.InstanceID != configs[0].InstanceID || c.MaxBundleBytes != configs[0].MaxBundleBytes || pathsOverlap(b.StateDir, c.SourceRoot) || pathsOverlap(b.StateDir, c.StateDir) || pathsOverlap(b.StateDir, b.Configs[i]) || pathsOverlap(b.StateDir, c.RcloneConfig) || pathsOverlap(b.StateDir, c.RcloneBinary) {
			return failure("offsite_configuration_invalid")
		}
		for j := 0; j < i; j++ {
			if pathsOverlap(c.StateDir, configs[j].StateDir) || binding(c) == binding(configs[j]) {
				return failure("offsite_configuration_invalid")
			}
		}
	}
	// Every mutable workspace must be disjoint from every protected input,
	// not just its own configuration. A provider can otherwise reclaim another
	// provider's credentials (or the top-level batch config) before using them.
	mutable := []string{b.StateDir}
	protected := append([]string{}, b.Configs...)
	if b.configPath != "" {
		protected = append(protected, b.configPath)
	}
	for _, c := range configs {
		mutable = append(mutable, c.StateDir)
		protected = append(protected, c.RcloneConfig, c.RcloneBinary)
	}
	for _, dir := range mutable {
		for _, path := range protected {
			if pathsOverlap(dir, path) {
				return failure("offsite_configuration_invalid")
			}
		}
	}
	if b.PhaseLease != "" {
		if !absolute(b.PhaseLease) {
			return failure("offsite_configuration_invalid")
		}
		for _, dir := range append(mutable, configs[0].SourceRoot) {
			if pathsOverlap(dir, b.PhaseLease) {
				return failure("offsite_configuration_invalid")
			}
		}
	}
	return nil
}
func pushBatch(ctx context.Context, b BatchConfig, configs []Config, transports []transport) (BatchResult, error) {
	var result BatchResult
	if ctx == nil || ctx.Err() != nil {
		return result, failure("offsite_cancelled")
	}
	if err := validateBatchConfiguration(b, configs); err != nil {
		return result, err
	}
	// One group owns preparation and serial verification. Old independent timer
	// units must be drained before enabling this command; their per-destination
	// locks remain an additional guard, not a global resource reservation.
	if err := privateDir(b.StateDir, true); err != nil {
		return result, err
	}
	lockPath := filepath.Join(b.StateDir, "batch.lock")
	if info, err := os.Lstat(lockPath); err == nil && !info.Mode().IsRegular() {
		return result, failure("offsite_state_invalid")
	}
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return result, failure("offsite_storage_unavailable")
	}
	defer f.Close()
	if lock(f) != nil {
		return result, failure("offsite_busy")
	}
	ctx, cancel := context.WithTimeout(ctx, 24*time.Hour)
	defer cancel()
	release, leaseErr := backupphase.Acquire(ctx, b.PhaseLease)
	if leaseErr != nil {
		if ctx.Err() != nil {
			return result, failure("offsite_cancelled")
		}
		return result, failure("offsite_storage_unavailable")
	}
	defer release()
	marker := configs[0]
	marker.StateDir = b.StateDir
	encoded, _ := json.Marshal(b.Configs)
	sum := sha256.Sum256(encoded)
	marker.Remote = "batch:" + hex.EncodeToString(sum[:])
	if err := batchHeadroom(b.StateDir, b.MinFreeBytes, 0); err != nil {
		return result, err
	}
	work, err := prepareWork(marker)
	if err != nil {
		return result, err
	}
	defer os.RemoveAll(work)
	shared := &batchPreparation{work: work, floor: b.MinFreeBytes}
	failed := false
	for i, c := range configs {
		if ctx.Err() != nil {
			return result, failure("offsite_cancelled")
		}
		var tr transport
		if transports != nil {
			tr = transports[i]
		}
		r := Result{}
		e := batchHeadroom(b.StateDir, b.MinFreeBytes, 0)
		if e == nil {
			e = batchHeadroom(existingParent(c.StateDir), b.MinFreeBytes, 0)
		}
		if e == nil {
			r, e = pushPrepared(ctx, c, tr, shared)
		}
		item := BatchDestinationResult{Index: i, Result: r}
		if e != nil {
			failed = true
			item.Reason = "offsite_failed"
			var ce *core.Error
			if errors.As(e, &ce) {
				item.Reason = ce.Code
			}
		}
		result.Destinations = append(result.Destinations, item)
	}
	if failed {
		return result, failure("offsite_batch_failed")
	}
	return result, nil
}

// Prospective phase admission, not an atomic reservation against unrelated
// writers. Already allocated shared and pending archives are in available space.
func batchHeadroom(path string, floor, bound int64) error {
	available, err := availableBytes(path)
	if err != nil {
		return failure("offsite_storage_unavailable")
	}
	if bound < 0 || bound > 2*maxBundleBytes || available < uint64(floor)+uint64(bound) {
		return failure("offsite_storage_headroom")
	}
	return nil
}

// New destination state directories may not exist yet. Inspect the filesystem
// of their nearest existing parent without creating or reclaiming any work.
func existingParent(path string) string {
	for {
		if _, err := os.Stat(path); err == nil {
			return path
		}
		parent := filepath.Dir(path)
		if parent == path {
			return path
		}
		path = parent
	}
}
