package reviewruntime

import (
	"context"
	"github.com/benhook1013/FireController/internal/reviewexec"
	"github.com/benhook1013/FireController/internal/reviewworkspace"
	"log"
	"time"
)

func startWorkspaceCleanup(ctx context.Context, manager *reviewworkspace.Manager, engine *reviewexec.Engine, capability string) (<-chan error, error) {
	return manager.StartPruning(ctx, func(ctx context.Context, id string) (bool, error) {
		return engine.WorkspaceDisposable(ctx, capability, id, time.Now())
	}, func(error) {
		log.Printf("review_workspace_cleanup_failed: terminal checkout cleanup will retry; review evidence is retained")
	})
}
func (d *CLIDriver) StartWorkspaceCleanup(ctx context.Context) (<-chan error, error) {
	return startWorkspaceCleanup(ctx, d.manager, d.engine, d.capability.ID)
}
func (d *HostedDriver) StartWorkspaceCleanup(ctx context.Context) (<-chan error, error) {
	return startWorkspaceCleanup(ctx, d.manager, d.engine, d.capability.ID)
}
