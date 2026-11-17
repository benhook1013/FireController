package reviewruntime

import (
	"context"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviewexec"
	"github.com/benhook1013/FireController/internal/reviewprocess"
	"github.com/benhook1013/FireController/internal/reviewworkspace"
)

// NewCLIDriverWithMaintenance composes an explicit trusted local maintenance
// callback before the driver is published. The ordinary constructor remains
// callback-free. This grants no cache ownership or deletion authority: the
// callback must enforce its own protected binding, custody and eligibility.
// There is no additional worker; StartWorkspaceCleanup owns the existing loop.
func NewCLIDriverWithMaintenance(store *core.Store, engine *reviewexec.Engine, capability OperatorCapability, manager *reviewworkspace.Manager, captures *CaptureStore, reader reviewexec.TargetReader, runner *reviewprocess.Runner, maintenance func(context.Context) error) (*CLIDriver, error) {
	if maintenance == nil {
		return nil, configurationError()
	}
	driver, err := NewCLIDriver(store, engine, capability, manager, captures, reader, runner)
	if err != nil {
		return nil, err
	}
	driver.workspaceMaintenance = maintenance
	return driver, nil
}
