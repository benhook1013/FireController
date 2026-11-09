package reviewexec

import (
	"errors"
	"testing"

	"github.com/benhook1013/FireController/internal/core"
)

type optionalCIGateReader struct {
	*ciWatchReader
	available bool
}

func (r *optionalCIGateReader) SupportsCIGate() bool { return r.available }

func TestCIWatchRejectsUnavailableOptionalGateBeforeRegistration(t *testing.T) {
	for _, available := range []bool{false, true} {
		options, _ := lifecycleOptions(nil)
		s := newLifecycleSchedule(options)
		reader := &optionalCIGateReader{ciWatchReader: &ciWatchReader{}, available: available}
		s.readers = map[string]TargetReader{"optional": reader}
		sub, err := s.registerCIWatch(core.Scope{PrincipalID: "principal-one"}, ciWatchBinding{capability: "optional"}, []CIWatchTarget{{PullNumber: 2}})
		if !available {
			var failure *core.Error
			if sub != nil || !errors.As(err, &failure) || failure.Code != "overloaded" || ciWatchDiagnosticReason(t, err) != "ci_watch_observer_unavailable" || len(s.watchers) != 0 || len(s.watches) != 0 || len(s.watchPrincipals) != 0 || reader.calls != 0 {
				t.Fatal("unsupported optional observer consumed watch capacity or provider work", sub, err)
			}
		} else {
			if err != nil || sub == nil || len(s.watchers) != 1 {
				t.Fatal(sub, err)
			}
			s.unregisterCIWatch(sub)
		}
	}
}
