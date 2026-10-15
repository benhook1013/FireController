package reviewexec

import (
	"context"
	"database/sql"

	"github.com/benhook1013/FireController/internal/core"
)

// ExecutionConfiguration describes stored project settings, not capability
// availability, permission to dispatch, review sufficiency or merge readiness.
type ExecutionConfiguration struct {
	ProjectID    string   `json:"project_id"`
	Revision     int64    `json:"revision"`
	CapabilityID string   `json:"capability_id"`
	Mode         string   `json:"mode"`
	Queue        []int64  `json:"queue"`
	Channels     []string `json:"channels"`
	Paused       bool     `json:"paused"`
}

func (e *Engine) ReadExecutionConfiguration(ctx context.Context, scope core.Scope) (ExecutionConfiguration, error) {
	if !identifier.MatchString(scope.ProjectID) {
		return ExecutionConfiguration{}, invalid()
	}
	var result ExecutionConfiguration
	err := e.store.Read(ctx, scope, func(ctx context.Context, tx *sql.Tx) error {
		configuration, revision, err := readConfiguration(ctx, tx, scope.ProjectID)
		if err != nil {
			return err
		}
		if revision < 1 || validateConfigurationShape(configuration) != nil {
			return core.Fail("storage_unavailable", "Stored review configuration is unavailable.", 500)
		}
		result = ExecutionConfiguration{ProjectID: scope.ProjectID, Revision: revision, CapabilityID: configuration.CapabilityID, Mode: configuration.Mode, Queue: configuration.Queue, Channels: configuration.Channels, Paused: configuration.Paused}
		return nil
	})
	if err != nil {
		return ExecutionConfiguration{}, err
	}
	return result, nil
}
