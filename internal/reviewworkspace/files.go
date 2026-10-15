package reviewworkspace

import (
	"bytes"
	"context"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

const changedFilesOutputLimit = 8 << 20
const changedFilesCountLimit = 10000

// ChangedFiles returns the complete portable source paths in the admitted
// merge-base-to-candidate comparison. It does not establish review coverage.
func (m *Manager) ChangedFiles(ctx context.Context, w Workspace) ([]string, error) {
	if m == nil || ctx == nil {
		return nil, errInput
	}
	ctx, cancel := context.WithTimeout(ctx, materializationBudget)
	defer cancel()
	if m.verify(ctx, w) != nil {
		return nil, errVerification
	}
	var output bytes.Buffer
	if m.run(ctx, w.Directory, &output, changedFilesOutputLimit, "diff", "--no-ext-diff", "--no-textconv", "--no-renames", "--name-only", "-z", w.MergeBaseSHA, w.CandidateSHA, "--") != nil {
		return nil, errVerification
	}
	data := output.Bytes()
	if len(data) != 0 && data[len(data)-1] != 0 || bytes.Count(data, []byte{0}) > changedFilesCountLimit {
		return nil, errVerification
	}
	paths := make([]string, 0)
	if len(data) != 0 {
		seen := make(map[string]bool)
		for _, raw := range bytes.Split(data[:len(data)-1], []byte{0}) {
			path := string(raw)
			if !portableChangedPath(path) {
				return nil, errVerification
			}
			if !seen[path] {
				seen[path] = true
				paths = append(paths, path)
			}
		}
	}
	sort.Strings(paths)
	if m.verify(ctx, w) != nil || ctx.Err() != nil {
		return nil, errVerification
	}
	return paths, nil
}

func portableChangedPath(path string) bool {
	if path == "" || !utf8.ValidString(path) || strings.ContainsAny(path, "\\:") || strings.IndexFunc(path, unicode.IsControl) >= 0 {
		return false
	}
	for _, segment := range strings.Split(path, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return false
		}
	}
	return true
}
