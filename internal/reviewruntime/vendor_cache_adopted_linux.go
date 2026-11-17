//go:build linux

package reviewruntime

import (
	"context"
	"errors"
	"github.com/benhook1013/FireController/internal/buildinfo"
	"github.com/benhook1013/FireController/internal/reviewexec"
	"github.com/benhook1013/FireController/internal/reviewprocess"
	"github.com/benhook1013/FireController/internal/reviewworkspace"
	"slices"
	"time"
)

// AdoptedVendorCacheMaintenance is explicit opt-in. It schedules nothing and
// cannot be constructed from an unknown/modified serving executable.
type AdoptedVendorCacheMaintenance struct {
	runner     *reviewprocess.Runner
	manager    *reviewworkspace.Manager
	engine     *reviewexec.Engine
	captures   *CaptureStore
	capability string
	binding    *reviewprocess.CacheMaintenanceBinding
}

func NewAdoptedVendorCacheMaintenance(runner *reviewprocess.Runner, manager *reviewworkspace.Manager, engine *reviewexec.Engine, captures *CaptureStore, capability OperatorCapability, bindingPath string) (*AdoptedVendorCacheMaintenance, error) {
	identity := buildinfo.Current("")
	if identity.SourceState != "clean" {
		return nil, errors.New("cache maintenance requires qualified serving source")
	}
	return newAdoptedVendorCacheMaintenance(runner, manager, engine, captures, capability, bindingPath, identity.SourceRevision)
}

// source is supplied by actual executable build metadata in the public factory;
// tests may exercise this composition with an explicitly synthetic source.
func newAdoptedVendorCacheMaintenance(runner *reviewprocess.Runner, manager *reviewworkspace.Manager, engine *reviewexec.Engine, captures *CaptureStore, capability OperatorCapability, bindingPath, source string) (*AdoptedVendorCacheMaintenance, error) {
	if runner == nil || manager == nil || engine == nil || captures == nil || !slices.Contains(capability.Channels, "cli") {
		return nil, errors.New("cache maintenance resources unavailable")
	}
	if !runner.MatchesCacheConfiguration(manager, capability.CLIExecutable, capability.AuthenticationEnvironment, time.Duration(capability.CLIAllowanceTimeoutSeconds)*time.Second) {
		return nil, errors.New("cache runner configuration mismatch")
	}
	fingerprint, err := capabilityFingerprint(capability)
	if err != nil || fingerprint != capability.Fingerprint || !engine.MatchesCapability(reviewexec.Capability{ID: capability.ID, Provider: "coderabbit", Account: capability.Account, Channels: capability.Channels, Fingerprint: capability.Fingerprint}) {
		return nil, errors.New("cache capability binding mismatch")
	}
	binding, err := reviewprocess.NewCacheMaintenanceBinding(bindingPath, source, fingerprint, capability.AuthenticationEnvironment["HOME"])
	if err != nil {
		return nil, err
	}
	return &AdoptedVendorCacheMaintenance{runner, manager, engine, captures, capability.ID, binding}, nil
}
func (m *AdoptedVendorCacheMaintenance) Retire(ctx context.Context, relative, attempt string, limit int64) (VendorCacheRetirement, error) {
	return m.retireAt(ctx, relative, attempt, limit, time.Now().UTC())
}

// retireAt is an internal observation-clock fixture seam; operational callers
// use Retire and cannot supply a future retention time.
func (m *AdoptedVendorCacheMaintenance) retireAt(ctx context.Context, relative, attempt string, limit int64, now time.Time) (VendorCacheRetirement, error) {
	if m == nil || m.binding == nil {
		return VendorCacheRetirement{}, errors.New("adopted cache maintenance unavailable")
	}
	return retireVendorCachePayloadsWithCustody(ctx, m.runner, m.manager, m.engine, m.captures, m.capability, relative, attempt, limit, now, nil, func(ctx context.Context, fn func(string) error) error {
		return m.runner.WithAdoptedCacheMaintenance(ctx, m.binding, fn)
	})
}
