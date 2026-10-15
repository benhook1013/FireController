package main

import (
	"context"
	"path/filepath"
	"slices"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviewexec"
	"github.com/benhook1013/FireController/internal/reviewprocess"
	"github.com/benhook1013/FireController/internal/reviewprovider"
	"github.com/benhook1013/FireController/internal/reviewruntime"
	"github.com/benhook1013/FireController/internal/reviewworkspace"
)

// Operator configuration is composed once, before accepting requests. Ordinary
// installations have no provider drivers or review-workspace directories.
func buildReviewExecution(store *core.Store, dataDir string, catalog reviewruntime.Catalog) (*reviewexec.Engine, map[string]reviewexec.Driver, error) {
	capabilities := make([]reviewexec.Capability, 0, len(catalog.Capabilities))
	for _, capability := range catalog.Capabilities {
		capabilities = append(capabilities, reviewexec.Capability{ID: capability.ID, Provider: "coderabbit", Account: capability.Account, Channels: capability.Channels, Fingerprint: capability.Fingerprint})
	}
	engine, err := reviewexec.New(store, capabilities, reviewexec.WithNativeProjection())
	if err != nil {
		return nil, nil, err
	}
	drivers := make(map[string]reviewexec.Driver, len(capabilities))
	if len(capabilities) == 0 {
		return engine, drivers, nil
	}
	root, err := filepath.Abs(dataDir)
	if err != nil {
		return nil, nil, core.Fail("configuration_invalid", "The private review data directory is unavailable.", 500)
	}
	var captures *reviewruntime.CaptureStore
	var hostedCaptures *reviewruntime.HostedCaptureStore
	for _, capability := range catalog.Capabilities {
		manager, err := reviewworkspace.New(filepath.Join(root, "review-workspaces", capability.ID), capability.GitExecutable)
		if err != nil {
			return nil, nil, err
		}
		provider, err := reviewprovider.NewGitHub(capability.GitHubCredential())
		if err != nil {
			return nil, nil, err
		}
		var cli *reviewruntime.CLIDriver
		var hosted *reviewruntime.HostedDriver
		if slices.Contains(capability.Channels, "cli") {
			if captures == nil {
				captures, err = reviewruntime.NewCaptureStore(filepath.Join(root, "review-captures"))
				if err != nil {
					return nil, nil, err
				}
			}
			runner, err := reviewprocess.New(manager, capability.CLIExecutable, capability.AuthenticationEnvironment)
			if err != nil {
				return nil, nil, err
			}
			cli, err = reviewruntime.NewCLIDriver(store, engine, capability, manager, captures, provider, runner)
			if err != nil {
				return nil, nil, err
			}
		}
		if slices.Contains(capability.Channels, "hosted") {
			if hostedCaptures == nil {
				hostedCaptures, err = reviewruntime.NewHostedCaptureStore(filepath.Join(root, "hosted-review-captures"))
				if err != nil {
					return nil, nil, err
				}
			}
			hosted, err = reviewruntime.NewHostedDriver(store, engine, capability, manager, hostedCaptures, provider)
			if err != nil {
				return nil, nil, err
			}
		}
		switch {
		case cli != nil && hosted != nil:
			drivers[capability.ID] = &reviewChannelDriver{cli: cli, hosted: hosted}
		case cli != nil:
			drivers[capability.ID] = cli
		case hosted != nil:
			drivers[capability.ID] = hosted
		default:
			return nil, nil, core.Fail("configuration_invalid", "The private review capability has no supported channel.", 500)
		}
	}
	return engine, drivers, nil
}

// Both channels share the original capability/account binding while retaining
// independent drivers and reservations. No channel falls back to another.
type reviewChannelDriver struct {
	cli    *reviewruntime.CLIDriver
	hosted *reviewruntime.HostedDriver
}

// The capability has one configured provider even when both review channels
// are enabled. Source reads do not fall back between adapters.
func (d *reviewChannelDriver) SourceReader() reviewexec.TargetReader {
	if d == nil {
		return nil
	}
	if d.cli != nil {
		return d.cli.SourceReader()
	}
	return d.hosted.SourceReader()
}

func (d *reviewChannelDriver) selectChannel(channel string) reviewexec.Driver {
	switch channel {
	case "cli":
		if d.cli != nil {
			return d.cli
		}
	case "hosted":
		if d.hosted != nil {
			return d.hosted
		}
	}
	return nil
}

func (d *reviewChannelDriver) Prepare(ctx context.Context, operation reviewexec.Operation, configuration reviewexec.Configuration, attempt reviewexec.Attempt) (reviewexec.Preparation, error) {
	driver := d.selectChannel(operation.Channel)
	if driver == nil {
		return reviewexec.Preparation{}, core.Fail("review_held", "The selected review channel is unavailable.", 409)
	}
	return driver.Prepare(ctx, operation, configuration, attempt)
}

func (d *reviewChannelDriver) Dispatch(ctx context.Context, operation reviewexec.Operation, attempt reviewexec.Attempt, record func(context.Context, string) error) (reviewexec.Result, error) {
	driver := d.selectChannel(operation.Channel)
	if driver == nil {
		return reviewexec.Result{State: "failed", Reason: "execution_binding_changed"}, nil
	}
	return driver.Dispatch(ctx, operation, attempt, record)
}

func (d *reviewChannelDriver) StartRecovery(ctx context.Context) (<-chan error, error) {
	return d.hosted.StartRecovery(ctx)
}
