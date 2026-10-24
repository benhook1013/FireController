package reviewworkspace

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// ComparisonSnapshot identifies the complete pinned comparison, not a selected path scope.
type ComparisonSnapshot struct {
	BaseSHA, CandidateSHA string
}

// ComparisonEquivalence records local proof without changing either original snapshot.
type ComparisonEquivalence struct {
	OriginalBaseTree, OriginalHeadTree, CurrentBaseTree, CurrentHeadTree    string
	OriginalMergeBaseSHA, CurrentMergeBaseSHA, PatchSHA256, InventorySHA256 string
}

// VerifyComparisonEquivalence permits graph movement only when both whole-tree
// pairs, canonical patches and complete raw inventories remain identical. It
// reads existing objects only; missing local objects cannot trigger a fetch.
func (m *Manager) VerifyComparisonEquivalence(ctx context.Context, repositoryID int64, original, current ComparisonSnapshot, expectedOriginalPatchSHA256 string) (ComparisonEquivalence, error) {
	if m == nil || ctx == nil || repositoryID <= 0 || !validSHA(original.BaseSHA) || !validSHA(original.CandidateSHA) || !validSHA(current.BaseSHA) || !validSHA(current.CandidateSHA) || !lowerHex(expectedOriginalPatchSHA256, 64) {
		return ComparisonEquivalence{}, errInput
	}
	cache, err := m.CachePath(repositoryID)
	if err != nil {
		return ComparisonEquivalence{}, errInput
	}
	ctx, cancel := context.WithTimeout(ctx, materializationBudget)
	defer cancel()
	if ctx.Err() != nil || m.layout() != nil || privateDirectory(cache, false) != nil {
		return ComparisonEquivalence{}, errVerification
	}
	// Keep the manager's shared environment unchanged. Pin even patch(), whose
	// canonical argument list is reused exactly, to this cache without discovery.
	local := *m
	local.env = append(append([]string(nil), m.env...), "GIT_DIR="+cache, "GIT_NO_LAZY_FETCH=1", "GIT_NO_REPLACE_OBJECTS=1", "GIT_ALLOW_PROTOCOL=")
	for _, check := range []struct{ argument, want string }{{"--is-bare-repository", "true"}, {"--is-shallow-repository", "false"}} {
		value, err := local.output(ctx, cache, "rev-parse", check.argument)
		if err != nil || value != check.want {
			return ComparisonEquivalence{}, errVerification
		}
	}
	commits := []string{original.BaseSHA, original.CandidateSHA, current.BaseSHA, current.CandidateSHA}
	trees := make([]string, len(commits))
	for i, sha := range commits {
		commit, err := local.output(ctx, cache, "rev-parse", "--verify", sha+"^{commit}")
		if err != nil || commit != sha {
			return ComparisonEquivalence{}, errVerification
		}
		trees[i], err = local.output(ctx, cache, "rev-parse", "--verify", sha+"^{tree}")
		if err != nil || !validSHA(trees[i]) {
			return ComparisonEquivalence{}, errVerification
		}
	}
	if trees[0] != trees[2] || trees[1] != trees[3] {
		return ComparisonEquivalence{}, errVerification
	}
	// Check reachable blobs and trees as well as the selected commits. A partial
	// clone remains local-only, and incomplete ancestry cannot prove a comparison.
	if _, err := local.output(ctx, cache, append([]string{"rev-list", "--objects", "--missing=error", "--quiet"}, commits...)...); err != nil {
		return ComparisonEquivalence{}, errVerification
	}
	proof := ComparisonEquivalence{OriginalBaseTree: trees[0], OriginalHeadTree: trees[1], CurrentBaseTree: trees[2], CurrentHeadTree: trees[3]}
	var originalInventory []byte
	for i, snapshot := range []ComparisonSnapshot{original, current} {
		mergeBase, err := local.output(ctx, cache, "merge-base", "--all", snapshot.BaseSHA, snapshot.CandidateSHA)
		if err != nil || !validSHA(mergeBase) {
			return ComparisonEquivalence{}, errVerification
		}
		w := Workspace{Directory: cache, MergeBaseSHA: mergeBase, CandidateSHA: snapshot.CandidateSHA}
		patch, err := local.patch(ctx, w)
		if err != nil || patch != expectedOriginalPatchSHA256 {
			return ComparisonEquivalence{}, errVerification
		}
		var inventory bytes.Buffer
		if local.run(ctx, cache, &inventory, patchLimit, "diff", "--no-ext-diff", "--no-textconv", "--no-color", "--no-relative", "--ignore-submodules=none", "--raw", "--no-abbrev", "--no-renames", "-z", mergeBase, snapshot.CandidateSHA, "--") != nil || !validComparisonInventory(inventory.Bytes()) {
			return ComparisonEquivalence{}, errVerification
		}
		if i == 0 {
			proof.OriginalMergeBaseSHA = mergeBase
			originalInventory = inventory.Bytes()
		} else {
			proof.CurrentMergeBaseSHA = mergeBase
			if !bytes.Equal(originalInventory, inventory.Bytes()) {
				return ComparisonEquivalence{}, errVerification
			}
		}
	}
	if ctx.Err() != nil {
		return ComparisonEquivalence{}, errVerification
	}
	proof.PatchSHA256 = expectedOriginalPatchSHA256
	hash := sha256.Sum256(originalInventory)
	proof.InventorySHA256 = hex.EncodeToString(hash[:])
	return proof, nil
}

func validComparisonInventory(data []byte) bool {
	if len(data) == 0 {
		return true
	}
	if data[len(data)-1] != 0 {
		return false
	}
	fields := bytes.Split(data[:len(data)-1], []byte{0})
	if len(fields)%2 != 0 {
		return false
	}
	seen := make(map[string]bool, len(fields)/2)
	for i := 0; i < len(fields); i += 2 {
		header, path := strings.Split(string(fields[i]), " "), string(fields[i+1])
		if len(header) != 5 || len(header[0]) != 7 || header[0][0] != ':' || !portableChangedPath(path) || seen[path] {
			return false
		}
		seen[path] = true
		oldMode, newMode, oldSHA, newSHA := header[0][1:], header[1], header[2], header[3]
		if !validComparisonMode(oldMode) || !validComparisonMode(newMode) || !validSHA(oldSHA) || !validSHA(newSHA) {
			return false
		}
		oldAbsent, newAbsent := oldMode == "000000", newMode == "000000"
		if oldAbsent != (oldSHA == strings.Repeat("0", 40)) || newAbsent != (newSHA == strings.Repeat("0", 40)) || oldMode == newMode && oldSHA == newSHA {
			return false
		}
		switch header[4] {
		case "A":
			if !oldAbsent || newAbsent {
				return false
			}
		case "D":
			if oldAbsent || !newAbsent {
				return false
			}
		case "M", "T":
			if oldAbsent || newAbsent || (oldMode[:3] == newMode[:3]) != (header[4] == "M") {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func validComparisonMode(mode string) bool {
	switch mode {
	case "000000", "100644", "100755", "120000", "160000":
		return true
	}
	return false
}
