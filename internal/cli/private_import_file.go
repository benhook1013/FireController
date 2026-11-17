package cli

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"runtime"

	"github.com/benhook1013/FireController/internal/client"
)

// readPrivateImportFile bounds private prepared input before profile access.
func readPrivateImportFile(ctx context.Context, path string, maximum int64) (json.RawMessage, error) {
	invalid := func() error {
		return &client.Error{Code: "invalid_import_file", Message: "Select an owner-private regular prepared JSON file within the size limit.", Action: "Correct the selected file locally and retry the same intended request.", Status: 400}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	private := func(info os.FileInfo) bool {
		return info != nil && info.Mode().IsRegular() && info.Size() <= maximum && (runtime.GOOS == "windows" || info.Mode().Perm()&0077 == 0)
	}
	before, err := os.Lstat(path)
	if err != nil || !private(before) {
		return nil, invalid()
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, invalid()
	}
	defer f.Close()
	after, err := f.Stat()
	if err != nil || !private(after) || !os.SameFile(before, after) {
		return nil, invalid()
	}
	pathAfter, err := os.Lstat(path)
	if err != nil || !private(pathAfter) || !os.SameFile(before, pathAfter) {
		return nil, invalid()
	}
	data, err := io.ReadAll(io.LimitReader(f, maximum+1))
	if err != nil || int64(len(data)) > maximum {
		return nil, invalid()
	}
	after, err = f.Stat()
	if err != nil || !private(after) || !os.SameFile(before, after) {
		return nil, invalid()
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	return data, nil
}
