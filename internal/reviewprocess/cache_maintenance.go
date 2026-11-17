package reviewprocess

import (
	"context"
	"errors"
	"strings"
)

// WithCacheMaintenance binds maintenance to this runner's configured HOME and
// shared producer custody. It does not establish exclusive installation ownership
// against an external CLI, or authorize deleting any particular cache artifact.
func (r *Runner) WithCacheMaintenance(ctx context.Context, fn func(string) error) error {
	if r == nil || r.cacheCustody == nil || r.homeLease == nil || fn == nil {
		return errors.New("CLI cache maintenance unavailable")
	}
	home := ""
	for _, entry := range r.env {
		if strings.HasPrefix(entry, "HOME=") {
			if home != "" {
				return errors.New("ambiguous CLI home")
			}
			home = strings.TrimPrefix(entry, "HOME=")
		}
	}
	if home == "" {
		return errors.New("CLI home unavailable")
	}
	return r.cacheCustody.TryMaintenance(ctx, func() error {
		lock, err := r.homeLease.acquire(true)
		if err != nil {
			return err
		}
		defer lock.Close()
		return fn(home)
	})
}
