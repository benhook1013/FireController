package reviewprocess

import (
	"context"
	"errors"
)

// WithAdoptedCacheMaintenance is the generation-qualified entry point. A
// pre-lock observation alone is insufficient: revoke may occur while waiting.
// Existing admitted work may finish; the generation owner must wait for HOME
// exclusion before introducing an unqualified replacement writer.
func (r *Runner) WithAdoptedCacheMaintenance(ctx context.Context, binding *CacheMaintenanceBinding, fn func(string) error) error {
	if r == nil || binding == nil || fn == nil {
		return errors.New("adopted cache maintenance unavailable")
	}
	if err := binding.Check(ctx); err != nil {
		return err
	}
	return r.WithCacheMaintenance(ctx, func(home string) error {
		if home != binding.home {
			return errors.New("cache generation HOME mismatch")
		}
		if err := binding.Check(ctx); err != nil {
			return err
		}
		return fn(home)
	})
}
