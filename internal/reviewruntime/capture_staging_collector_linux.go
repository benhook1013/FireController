//go:build linux

package reviewruntime

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/benhook1013/FireController/internal/capturecustody"
)

// PublishedCaptureBinding comes from retained native capture references, never
// filename discovery. A binding is revalidated through the store's strict Read.
type PublishedCaptureBinding struct {
	AttemptID string
	Reference CaptureReference
}

type CaptureStagingCollection struct{ Examined, Eligible, Removed int }

type stagingRemoval struct {
	stage, final string
	identity     os.FileInfo
}

// CollectPublishedCaptureStaging is an uninstalled Linux collector prototype.
// It defaults to dry-run (apply=false). Actual adoption of a lease-aware writer
// generation and root ownership must be qualified before any installed caller.
// It removes only redundant new-generation hard-link names, not capture bytes.
// Sole-copy, legacy, malformed and unbound files are never deletion candidates.
func CollectPublishedCaptureStaging(ctx context.Context, root, kind string, bindings []PublishedCaptureBinding, apply bool) (CaptureStagingCollection, error) {
	var result CaptureStagingCollection
	if ctx == nil || len(bindings) == 0 || len(bindings) > 128 {
		return result, captureError("validation_failed")
	}
	switch kind {
	case "cli", "hosted", "exhaustion":
	default:
		return result, captureError("validation_failed")
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	custody, err := capturecustody.Collector(ctx, root)
	if err != nil {
		return result, err
	}
	defer custody.Close()
	// Validate the complete bounded binding set before scanning or modifying names.
	finals := make(map[string]os.FileInfo)
	refs := make(map[string]CaptureReference)
	var verifiedBytes int64
	for _, binding := range bindings {
		if ctx.Err() != nil {
			return result, ctx.Err()
		}
		if !validIdentifier(binding.AttemptID) || !validHostedReference(binding.Reference) {
			return result, captureError("validation_failed")
		}
		name := binding.AttemptID + ".json"
		if kind == "hosted" {
			name = hostedCaptureFilename(binding.AttemptID, binding.Reference)
		}
		if _, seen := finals[name]; seen {
			if refs[name] != binding.Reference {
				return result, captureError("validation_failed")
			}
			continue
		}
		path := filepath.Join(root, name)
		info, e := os.Lstat(path)
		if e != nil || !privateRegular(info) || !captureMode(info, 0600) {
			return result, captureError("capture_unavailable")
		}
		verifiedBytes += info.Size()
		if verifiedBytes > 64<<20 {
			return result, captureError("validation_failed")
		}
		switch kind {
		case "cli":
			_, ref, e := (&CaptureStore{root: root}).Read(ctx, binding.AttemptID)
			if e != nil || ref != binding.Reference {
				return result, captureError("capture_unavailable")
			}
		case "hosted":
			if _, e := (&HostedCaptureStore{root: root}).Read(ctx, binding.AttemptID, binding.Reference); e != nil {
				return result, e
			}
		case "exhaustion":
			data, e := readCaptureStagingExhaustion(ctx, root, binding)
			if e != nil || captureReference(data) != binding.Reference {
				return result, captureError("capture_unavailable")
			}
		}
		after, e := os.Lstat(path)
		if e != nil || !sameStagingIdentity(info, after) {
			return result, captureError("capture_unavailable")
		}
		finals[name] = after
		refs[name] = binding.Reference
	}
	directory, err := os.Open(root)
	if err != nil {
		return result, err
	}
	defer directory.Close()
	var entries []os.DirEntry
	for {
		batch, e := directory.ReadDir(256)
		entries = append(entries, batch...)
		if len(entries) > 4096 || ctx.Err() != nil {
			return result, captureError("capture_unavailable")
		}
		if errors.Is(e, io.EOF) {
			break
		}
		if e != nil {
			return result, e
		}
	}
	var plan []stagingRemoval
	prefix := ".capture-stage-v1-" + kind + "-"
	for _, entry := range entries {
		result.Examined++
		if !strings.HasPrefix(entry.Name(), prefix) || len(entry.Name()) == len(prefix) {
			continue
		}
		info, e := entry.Info()
		if e != nil {
			return result, e
		}
		if !privateRegular(info) || !captureMode(info, 0600) {
			continue
		}
		for name, final := range finals {
			if os.SameFile(info, final) {
				plan = append(plan, stagingRemoval{entry.Name(), name, info})
				break
			}
		}
		if len(plan) > 128 || ctx.Err() != nil {
			return result, captureError("capture_unavailable")
		}
	}
	if ctx.Err() != nil {
		return result, ctx.Err()
	}
	if err = custody.Validate(); err != nil {
		return result, err
	}
	result.Eligible = len(plan)
	if !apply {
		return result, nil
	}
	for _, item := range plan {
		if ctx.Err() != nil {
			return result, ctx.Err()
		}
		if err = custody.Validate(); err != nil {
			return result, err
		}
		stage, e := os.Lstat(filepath.Join(root, item.stage))
		final, f := os.Lstat(filepath.Join(root, item.final))
		if e != nil || f != nil || !sameStagingIdentity(item.identity, stage) || !sameStagingIdentity(item.identity, final) {
			return result, captureError("capture_unavailable")
		}
		if err = os.Remove(filepath.Join(root, item.stage)); err != nil {
			return result, err
		}
		result.Removed++
		if err = syncCaptureDirectory(root); err != nil {
			return result, err
		}
	}
	return result, nil
}

func sameStagingIdentity(a, b os.FileInfo) bool {
	return privateRegular(b) && captureMode(b, 0600) && os.SameFile(a, b) && a.Size() == b.Size() && a.ModTime() == b.ModTime()
}

func readCaptureStagingExhaustion(ctx context.Context, root string, b PublishedCaptureBinding) ([]byte, error) {
	record, found, err := (&CLIExhaustionStore{root: root}).LoadCLIExhaustion(ctx, b.AttemptID)
	if err != nil || !found {
		return nil, captureError("capture_unavailable")
	}
	return exhaustionEnvelope(record)
}
