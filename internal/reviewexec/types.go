// Package reviewexec owns the durable local request and attempt journal. Provider
// calls and subprocess waits never run inside its transaction callbacks.
package reviewexec

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"regexp"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/benhook1013/FireController/internal/core"
)

// Capability names an operator-owned adapter/account without exposing secrets.
// Clients cannot supply capabilities, executable paths or provider identities.
type Capability struct {
	ID, Provider, Account string
	Channels              []string
	Fingerprint           string
}

type Configuration struct {
	ExpectedRevision *int64   `json:"expected_revision"`
	CapabilityID     string   `json:"capability_id"`
	Mode             string   `json:"mode"`
	Queue            []int64  `json:"queue"`
	Channels         []string `json:"channels"`
	Paused           bool     `json:"paused"`
}

type Request struct {
	ExpectedConfigurationRevision int64  `json:"expected_configuration_revision"`
	PullNumber                    int64  `json:"pull_number"`
	Channel                       string `json:"channel"`
	HeadSHA                       string `json:"head_sha"`
}

type Receipt struct {
	ID       string `json:"id"`
	State    string `json:"state"`
	Revision int64  `json:"revision"`
}

// Anchor is supplied only by the trusted admission worker after fresh provider
// and Git checks. It never derives applicability from an old count or title.
type Anchor struct {
	RepositoryID     int64  `json:"repository_id"`
	Repository       string `json:"repository"`
	PullNumber       int64  `json:"pull_number"`
	CandidateSHA     string `json:"candidate_sha"`
	BaseSHA          string `json:"base_sha"`
	ParentSHA        string `json:"parent_sha"`
	ParentPullNumber int64  `json:"parent_pull_number"`
	BaseRef          string `json:"base_ref"`
	MergeBaseSHA     string `json:"merge_base_sha"`
	PatchSHA256      string `json:"patch_sha256"`
	ObservedAt       string `json:"observed_at"`
}

type Operation struct {
	ID                    string `json:"id"`
	ProjectID             string `json:"project_id"`
	ActorID               string `json:"actor_id"`
	WorkerID              string `json:"worker_id"`
	ConfigurationRevision int64  `json:"configuration_revision"`
	PolicyRevision        int64  `json:"policy_revision"`
	CapabilityFingerprint string `json:"capability_fingerprint"`
	PullNumber            int64  `json:"pull_number"`
	Channel               string `json:"channel"`
	HeadSHA               string `json:"head_sha"`
	State                 string `json:"state"`
	Revision              int64  `json:"revision"`
	Reason                string `json:"reason"`
	AttemptID             string `json:"attempt_id"`
	CreatedAt             string `json:"created_at"`
	UpdatedAt             string `json:"updated_at"`
}

type Attempt struct {
	ID          string  `json:"id"`
	OperationID string  `json:"operation_id"`
	ProjectID   string  `json:"project_id"`
	Provider    string  `json:"provider"`
	Account     string  `json:"account"`
	State       string  `json:"state"`
	Revision    int64   `json:"revision"`
	Anchor      *Anchor `json:"anchor"`
	Intent      string  `json:"-"`
	ExternalID  string  `json:"external_id"`
}

type Engine struct {
	routeNow                   func() time.Time
	checkpointWake             chan struct{}
	checkpointRunning          atomic.Bool
	checkpointMu               sync.Mutex
	checkpointWaiters          map[string]map[*checkpointWaiter]struct{}
	checkpointWaiterPrincipals map[string]int
	checkpointWaiterCount      int
	store                      *core.Store
	capabilities               map[string]Capability
	wake                       chan struct{}
	sourceWake                 chan struct{}
	sourceRunning              atomic.Bool
	sourceMu                   sync.Mutex
	sourceWaiters              map[string]map[*sourceRefreshWaiter]struct{}
	sourceWaiterPrincipals     map[string]int
	sourceWaiterCount          int
	running                    atomic.Bool
	publishAdjudication        func(context.Context, *sql.Tx, core.Scope, ReviewAdjudication) error
	publishAttempt             func(context.Context, *sql.Tx, Attempt) error
}

// EngineOption composes trusted application integrations before an engine is
// exposed to handlers or supervised workers.
type EngineOption func(*Engine)

var identifier = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
var sha = regexp.MustCompile(`^[0-9a-f]{40}$`)
var hash = regexp.MustCompile(`^[0-9a-f]{64}$`)
var repository = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,100}/[A-Za-z0-9_.-]{1,100}$`)

func New(store *core.Store, capabilities []Capability, options ...EngineOption) (*Engine, error) {
	e := &Engine{checkpointWake: make(chan struct{}, 1), checkpointWaiters: make(map[string]map[*checkpointWaiter]struct{}), checkpointWaiterPrincipals: make(map[string]int), store: store, capabilities: map[string]Capability{}, wake: make(chan struct{}, 1), sourceWake: make(chan struct{}, 1), sourceWaiters: make(map[string]map[*sourceRefreshWaiter]struct{}), sourceWaiterPrincipals: make(map[string]int)}
	for _, input := range capabilities {
		c, err := normalizedCapability(input)
		if err != nil {
			return nil, err
		}
		if _, exists := e.capabilities[c.ID]; exists {
			return nil, invalid()
		}
		e.capabilities[c.ID] = c
	}
	for _, option := range options {
		if option == nil {
			return nil, invalid()
		}
		option(e)
	}
	return e, nil
}

// MatchesCapability verifies trusted runtime composition against the same
// canonical binding used by New. It performs no storage or provider work.
func (e *Engine) MatchesCapability(expected Capability) bool {
	if e == nil {
		return false
	}
	current, exists := e.capabilities[expected.ID]
	normalized, err := normalizedCapability(expected)
	return exists && err == nil && current.Fingerprint == normalized.Fingerprint
}

func normalizedCapability(c Capability) (Capability, error) {
	if !identifier.MatchString(c.ID) || !identifier.MatchString(c.Provider) || !identifier.MatchString(c.Account) || len(c.Channels) == 0 || len(c.Channels) > 2 {
		return Capability{}, invalid()
	}
	seen := map[string]bool{}
	for _, channel := range c.Channels {
		if !validChannel(channel) || seen[channel] {
			return Capability{}, invalid()
		}
		seen[channel] = true
	}
	c.Channels = append([]string(nil), c.Channels...)
	slices.Sort(c.Channels)
	if c.Fingerprint != "" && !hash.MatchString(c.Fingerprint) {
		return Capability{}, invalid()
	}
	canonical := struct {
		ID, Provider, Account    string
		Channels                 []string
		ConfigurationFingerprint string
	}{c.ID, c.Provider, c.Account, c.Channels, c.Fingerprint}
	digest := sha256.Sum256([]byte(encoded(canonical)))
	c.Fingerprint = hex.EncodeToString(digest[:])
	return c, nil
}

func validChannel(s string) bool { return s == "cli" || s == "hosted" }
func invalid() error {
	return core.Fail("validation_failed", "Use valid bounded review execution fields.", http.StatusBadRequest)
}
func conflict() error {
	return core.Fail("review_conflict", "The review state changed; read its current revision before continuing.", http.StatusConflict)
}
func stamp() string        { return time.Now().UTC().Format(time.RFC3339Nano) }
func encoded(v any) string { b, _ := json.Marshal(v); return string(b) }

func actorAllowed(ctx context.Context, tx *sql.Tx, project, actor, worker string) (bool, error) {
	var n int
	err := tx.QueryRowContext(ctx, `SELECT count(*) FROM credentials c WHERE c.principal_id=? AND c.revoked=0 AND (c.is_admin=1 OR EXISTS(SELECT 1 FROM grants g WHERE g.credential_id=c.id AND g.project_id=? AND g.can_write=1)) AND (?='' OR EXISTS(SELECT 1 FROM workers w WHERE w.project_id=? AND w.id=?))`, actor, project, worker, project, worker).Scan(&n)
	return n == 1, err
}
