package cli

import (
	"context"
	"errors"
	"io"
	"os"
	"runtime"
)

const ownerPrivateJSONLimit = 256 << 10

var errInvalidOwnerPrivateJSONFile = errors.New("invalid owner-private JSON file")

// readOwnerPrivateJSON admits one bounded owner-private regular file while
// checking that the selected path still names the descriptor being read.
// Callers retain responsibility for their document-specific validation and
// safe public diagnostic.
func readOwnerPrivateJSON(ctx context.Context, path string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	private := func(info os.FileInfo) bool {
		return info != nil && info.Mode().IsRegular() && info.Size() <= ownerPrivateJSONLimit && (runtime.GOOS == "windows" || info.Mode().Perm()&0077 == 0)
	}
	before, err := os.Lstat(path)
	if err != nil || !private(before) {
		return nil, errInvalidOwnerPrivateJSONFile
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, errInvalidOwnerPrivateJSONFile
	}
	defer f.Close()
	actual, err := f.Stat()
	if err != nil || !private(actual) || !os.SameFile(before, actual) {
		return nil, errInvalidOwnerPrivateJSONFile
	}
	pathAfter, err := os.Lstat(path)
	if err != nil || !private(pathAfter) || !os.SameFile(before, pathAfter) {
		return nil, errInvalidOwnerPrivateJSONFile
	}
	document, err := io.ReadAll(io.LimitReader(f, ownerPrivateJSONLimit+1))
	if err != nil || len(document) > ownerPrivateJSONLimit {
		return nil, errInvalidOwnerPrivateJSONFile
	}
	actual, err = f.Stat()
	if err != nil || !private(actual) || !os.SameFile(before, actual) {
		return nil, errInvalidOwnerPrivateJSONFile
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	return document, nil
}
