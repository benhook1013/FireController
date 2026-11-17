package reviewprovider

import (
	"slices"
	"strings"
	"sync"
)

const immutableMergeCacheCapacity = 128

// This cache belongs to one credential-bound adapter. Only validated exact Git
// object identities enter it; mutable PR/base/CI/permission reads never do.
// FIFO eviction bounds retention, and copies prevent caller aliasing.
type immutableMergeCache struct {
	mu     sync.Mutex
	values map[string]githubCIMerge
	order  []string
}

func (c *immutableMergeCache) get(repository, sha string) (githubCIMerge, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	value, ok := c.values[strings.ToLower(repository)+"/"+sha]
	value.Parents = slices.Clone(value.Parents)
	return value, ok
}
func (c *immutableMergeCache) put(repository string, value githubCIMerge) {
	if !validRepositorySelector(repository) || !lowerCISHA(value.SHA) || len(value.Parents) != 2 || !lowerCISHA(value.Parents[0].SHA) || !lowerCISHA(value.Parents[1].SHA) || value.Parents[0].SHA == value.Parents[1].SHA {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	key := strings.ToLower(repository) + "/" + value.SHA
	if _, exists := c.values[key]; exists {
		return
	}
	if c.values == nil {
		c.values = make(map[string]githubCIMerge)
	}
	if len(c.order) == immutableMergeCacheCapacity {
		delete(c.values, c.order[0])
		c.order = c.order[1:]
	}
	value.Parents = slices.Clone(value.Parents)
	c.values[key] = value
	c.order = append(c.order, key)
}
