package main

import (
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
		if !slices.Equal(capability.Channels, []string{"cli"}) {
			return nil, nil, core.Fail("configuration_invalid", "This release supports native CLI execution only; configure each runnable capability with the cli channel.", 500)
		}
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
	captures, err := reviewruntime.NewCaptureStore(filepath.Join(root, "review-captures"))
	if err != nil {
		return nil, nil, err
	}
	for _, capability := range catalog.Capabilities {
		manager, err := reviewworkspace.New(filepath.Join(root, "review-workspaces", capability.ID), capability.GitExecutable)
		if err != nil {
			return nil, nil, err
		}
		runner, err := reviewprocess.New(manager, capability.CLIExecutable, capability.AuthenticationEnvironment)
		if err != nil {
			return nil, nil, err
		}
		provider, err := reviewprovider.NewGitHub(capability.GitHubCredential())
		if err != nil {
			return nil, nil, err
		}
		driver, err := reviewruntime.NewCLIDriver(store, engine, capability, manager, captures, provider, runner)
		if err != nil {
			return nil, nil, err
		}
		drivers[capability.ID] = driver
	}
	return engine, drivers, nil
}
