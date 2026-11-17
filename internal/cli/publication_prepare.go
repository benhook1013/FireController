package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"

	"github.com/benhook1013/FireController/internal/client"
)

type publicationPreparedReceipt struct {
	ExpectedRevision int64                     `json:"expected_revision"`
	PreviewSHA256    string                    `json:"preview_sha256"`
	PreparedFile     string                    `json:"prepared_file"`
	Changes          client.PublicationChanges `json:"changes"`
}

func publicationOutputFailure(code string) error {
	return &client.Error{Code: code, Message: "The prepared publication file could not be saved to the selected path.", Action: "Choose a new absolute output path in an existing directory. Publication was not changed; existing files are never replaced.", Status: 400}
}

func emitPreparedPublication(stdout, stderr io.Writer, value any) int {
	raw, err := json.Marshal(value)
	if err == nil {
		raw = append(raw, '\n')
		var n int
		n, err = stdout.Write(raw)
		if err == nil && n != len(raw) {
			err = io.ErrShortWrite
		}
	}
	if err != nil {
		return report(stderr, ExitConfig, "prepared_receipt_output_failed", "The prepared file was saved but its receipt could not be written.", "Keep the selected output file and run publication preview --file with it to recover its preview hash. Publication was not changed.")
	}
	return ExitSuccess
}

func preparePublicationOutput(in *publicationInvocation) error {
	if _, err := os.Lstat(in.output); !errors.Is(err, os.ErrNotExist) {
		return publicationOutputFailure("prepared_file_exists")
	}
	parent, err := os.Stat(filepath.Dir(in.output))
	if err != nil || !parent.IsDir() {
		return publicationOutputFailure("prepared_file_failed")
	}
	// Bound the complete model-facing receipt before credentials or requests.
	receipt := publicationPreparedReceipt{in.changes.ExpectedRevision, string(make([]byte, 64)), in.output, in.changes}
	raw, err := json.Marshal(receipt)
	if err != nil || len(raw) > 16<<10 {
		return publicationOutputFailure("prepared_receipt_too_large")
	}
	return nil
}

func executePublicationPreparation(ctx context.Context, c *client.Client, in *publicationInvocation) (publicationPreparedReceipt, error) {
	input, sha, err := c.PreparePublication(ctx, in.changes)
	if err != nil {
		return publicationPreparedReceipt{}, err
	}
	data, err := json.Marshal(input)
	if err != nil {
		return publicationPreparedReceipt{}, publicationOutputFailure("prepared_file_failed")
	}
	data = append(data, '\n')
	if err = savePreparedPublication(ctx, in.output, data); err != nil {
		return publicationPreparedReceipt{}, err
	}
	return publicationPreparedReceipt{input.ExpectedRevision, sha, in.output, in.changes}, nil
}

// Stage complete bytes privately and then link them atomically into the new
// path. The final path is never replaced, including if it becomes a symlink.
func savePreparedPublication(ctx context.Context, path string, data []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".fc-publication-*")
	if err != nil {
		return publicationOutputFailure("prepared_file_failed")
	}
	defer f.Close()
	defer os.Remove(f.Name())
	if err = f.Chmod(0600); err == nil {
		var n int
		n, err = f.Write(data)
		if err == nil && n != len(data) {
			err = io.ErrShortWrite
		}
	}
	if err == nil {
		err = f.Sync()
	}
	closed := f.Close()
	if err != nil || closed != nil {
		return publicationOutputFailure("prepared_file_failed")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.Link(f.Name(), path); err != nil {
		if errors.Is(err, os.ErrExist) {
			return publicationOutputFailure("prepared_file_exists")
		}
		return publicationOutputFailure("prepared_file_failed")
	}
	// A final file already exists on later durability failure. Preserve it so
	// the operator can inspect/re-preview it rather than silently rebuilding.
	if runtime.GOOS != "windows" {
		dir, err := os.Open(filepath.Dir(path))
		if err == nil {
			err = dir.Sync()
			_ = dir.Close()
		}
		if err != nil {
			return &client.Error{Code: "prepared_file_uncertain", Message: "The prepared file exists but directory durability could not be confirmed.", Action: "Inspect the selected file and run publication preview with it; do not overwrite it. Publication was not changed."}
		}
	}
	return nil
}
