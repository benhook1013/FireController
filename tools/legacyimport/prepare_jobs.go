package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/benhook1013/FireController/internal/strictjson"
	"github.com/benhook1013/FireController/tools/legacyimport/internal/legacy"
)

const prepareUsage = `Usage: legacyimport prepare-jobs --snapshot ABSOLUTE_PATH --selection ABSOLUTE_JSON --output ABSOLUTE_JSON

Prepare selected current jobs and explicitly selected checkpoints from a sealed
legacy export. The private output is a native import batch; no service is called.
Optional --timeout bounds the whole preparation (default 30s, maximum 5m).
`

func uniqueFlags(flags *flag.FlagSet, args []string) bool {
	seen := map[string]bool{}
	for i := 0; i < len(args); i++ {
		if !strings.HasPrefix(args[i], "--") || args[i] == "--" {
			return false
		}
		name, value, assigned := strings.Cut(strings.TrimPrefix(args[i], "--"), "=")
		if flags.Lookup(name) == nil || seen[name] {
			return false
		}
		seen[name] = true
		if !assigned {
			i++
			if i >= len(args) {
				return false
			}
			value = args[i]
		}
		if value == "" {
			return false
		}
	}
	return flags.Parse(args) == nil && flags.NArg() == 0
}

func privateFile(path string, maximum int64) ([]byte, error) {
	if !filepath.IsAbs(path) {
		return nil, errors.New("invalid file")
	}
	before, err := os.Lstat(path)
	if err != nil || !before.Mode().IsRegular() || before.Size() > maximum || runtime.GOOS != "windows" && before.Mode().Perm()&0077 != 0 {
		return nil, errors.New("invalid file")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	actual, err := f.Stat()
	if err != nil || !os.SameFile(before, actual) || actual.Size() > maximum {
		return nil, errors.New("invalid file")
	}
	body, err := io.ReadAll(io.LimitReader(f, maximum+1))
	if err != nil || int64(len(body)) > maximum {
		return nil, errors.New("invalid file")
	}
	return body, nil
}

func publishPrepared(path string, body []byte) error {
	if !filepath.IsAbs(path) {
		return errors.New("invalid output")
	}
	parent := filepath.Dir(path)
	info, err := os.Lstat(parent)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || runtime.GOOS != "windows" && info.Mode().Perm()&0077 != 0 {
		return errors.New("invalid output")
	}
	if old, err := privateFile(path, 256<<10); err == nil {
		if bytes.Equal(old, body) {
			return nil
		}
		return errors.New("output conflict")
	}
	f, err := os.CreateTemp(parent, ".legacyimport-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	defer f.Close()
	if _, err = f.Write(body); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Link(name, path); err != nil {
		if old, e := privateFile(path, 256<<10); e == nil && bytes.Equal(old, body) {
			return nil
		}
		return err
	}
	if runtime.GOOS != "windows" {
		dir, e := os.Open(parent)
		if e != nil {
			return e
		}
		defer dir.Close()
		if e = dir.Sync(); e != nil {
			return e
		}
	}
	return nil
}

func runPrepare(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 1 && args[0] == "--help" {
		if _, err := io.WriteString(stdout, prepareUsage); err != nil {
			return 3
		}
		return 0
	}
	flags := flag.NewFlagSet("legacyimport prepare-jobs", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	snapshot := flags.String("snapshot", "", "")
	selectionPath := flags.String("selection", "", "")
	output := flags.String("output", "", "")
	budget := flags.Duration("timeout", 30*time.Second, "")
	if !uniqueFlags(flags, args) || !filepath.IsAbs(*snapshot) || !filepath.IsAbs(*selectionPath) || !filepath.IsAbs(*output) || *budget <= 0 || *budget > 5*time.Minute {
		diagnostic(stderr, "arguments_invalid", "Use prepare-jobs with absolute snapshot, selection and output paths and a timeout of at most five minutes.")
		return 2
	}
	request, cancel := context.WithTimeout(ctx, *budget)
	defer cancel()
	data, err := privateFile(*selectionPath, 64<<10)
	var selection legacy.JobSelection
	if err != nil || strictjson.Decode(data, &selection) != nil {
		diagnostic(stderr, "selection_invalid", "Supply a private regular strict JSON selection within 64 KiB.")
		return 3
	}
	batch, err := legacy.PrepareJobs(request, *snapshot, legacy.Limits{}, selection)
	if err != nil {
		if errors.Is(request.Err(), context.Canceled) {
			diagnostic(stderr, "preparation_canceled", "Job preparation was canceled.")
			return 130
		}
		if errors.Is(request.Err(), context.DeadlineExceeded) {
			diagnostic(stderr, "preparation_timed_out", "Job preparation exceeded its time budget.")
			return 3
		}
		var safe *legacy.Error
		if errors.As(err, &safe) && safe != nil {
			diagnostic(stderr, safe.Code, safe.Message)
		} else {
			diagnostic(stderr, "preparation_failed", "The selected source could not be prepared.")
		}
		return 3
	}
	body, err := jsonBytes(batch)
	if err != nil {
		diagnostic(stderr, "preparation_failed", "The native batch could not be encoded.")
		return 3
	}
	if request.Err() != nil {
		diagnostic(stderr, "preparation_interrupted", "Job preparation was interrupted before publication.")
		return 3
	}
	if err = publishPrepared(*output, body); err != nil {
		diagnostic(stderr, "output_unavailable", "The private output could not be published or already differs. Inspect the selected output; retry with the same selection and output path.")
		return 3
	}
	checkpoints := 0
	for _, item := range batch.Jobs {
		checkpoints += len(item.Checkpoints)
	}
	result := struct {
		Kind         string `json:"kind"`
		SourceSHA256 string `json:"source_sha256"`
		Jobs         int    `json:"jobs"`
		Checkpoints  int    `json:"checkpoints"`
	}{"prepared_job_import", batch.SourceSHA256, len(batch.Jobs), checkpoints}
	encoded, err := jsonBytes(result)
	if err != nil {
		return 3
	}
	if n, e := stdout.Write(encoded); e != nil || n != len(encoded) {
		diagnostic(stderr, "output_unavailable", "The prepared file may be available; inspect the output or retry the same selection and path.")
		return 3
	}
	return 0
}

func jsonBytes(value any) ([]byte, error) {
	b, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}
