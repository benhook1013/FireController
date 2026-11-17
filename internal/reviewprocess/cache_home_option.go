package reviewprocess

import "strings"

// WithCacheHomeLease is explicit opt-in; no daemon flag adopts it yet.
func WithCacheHomeLease(lease *CacheHomeLease) Option {
	return func(r *Runner) error {
		if lease == nil || r.cacheCustody == nil {
			return processError("configuration_invalid", false)
		}
		found := false
		for _, e := range r.env {
			if strings.HasPrefix(e, "HOME=") {
				if found || strings.TrimPrefix(e, "HOME=") != lease.home {
					return processError("configuration_invalid", false)
				}
				found = true
			}
		}
		if !found {
			return processError("configuration_invalid", false)
		}
		r.homeLease = lease
		return nil
	}
}
