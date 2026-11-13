// Package githubhook authenticates GitHub delivery hints. Payloads never supply
// retained provider facts: the callback only queues fresh, scoped acquisition.
package githubhook

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"mime"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/benhook1013/FireController/internal/strictjson"
)

const MaxBodyBytes = 4 << 20
const deliveryLimit = 4096
const deliveryTTL = 10 * time.Minute

// Hint contains selectors only. BranchRef is a bare branch name from a push;
// HeadSHA is empty for a deleted branch. Metadata and CI select refresh lanes,
// never observed outcomes. Missing PR numbers are not a complete
// repository/PR inventory. The receiver owns scope, coalescing and fresh reads.
type Hint struct {
	RepositoryID int64
	Metadata     bool
	CI           bool
	PullNumbers  []int64
	HeadSHA      string
	BaseRef      string
	BranchRef    string
}
type Callback func(context.Context, Hint) error

type delivery struct {
	event   string
	digest  [sha256.Size]byte
	done    bool
	expires time.Time
}
type Handler struct {
	secret     []byte
	callback   Callback
	mu         sync.Mutex
	deliveries map[string]*delivery
	now        func() time.Time
}

// New copies an explicit private secret (32..4096 bytes). Its callback must
// promptly queue the hint or return an error; it must not acquire provider facts.
func New(secret []byte, callback Callback) (*Handler, error) {
	if len(secret) < 32 || len(secret) > 4096 || callback == nil {
		return nil, errors.New("githubhook requires a 32..4096-byte secret and a callback")
	}
	return &Handler{secret: append([]byte(nil), secret...), callback: callback, deliveries: map[string]*delivery{}, now: time.Now}, nil
}

var deliveryID = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[1-8][0-9a-fA-F]{3}-[89abAB][0-9a-fA-F]{3}-[0-9a-fA-F]{12}$`)

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "Use POST.", http.StatusMethodNotAllowed)
		return
	}
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != "application/json" || r.Header.Get("Content-Encoding") != "" {
		http.Error(w, "Use uncompressed JSON.", http.StatusUnsupportedMediaType)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, MaxBodyBytes+1))
	if err != nil {
		http.Error(w, "The delivery body is unavailable.", http.StatusBadRequest)
		return
	}
	if len(body) > MaxBodyBytes {
		http.Error(w, "The delivery body is too large.", http.StatusRequestEntityTooLarge)
		return
	}
	signatures := r.Header.Values("X-Hub-Signature-256")
	if len(signatures) != 1 || !validSignature(h.secret, body, signatures[0]) {
		http.Error(w, "Invalid delivery signature.", http.StatusUnauthorized)
		return
	}
	ids, events := r.Header.Values("X-GitHub-Delivery"), r.Header.Values("X-GitHub-Event")
	if len(ids) != 1 || !deliveryID.MatchString(ids[0]) || len(events) != 1 || len(events[0]) < 1 || len(events[0]) > 64 {
		http.Error(w, "Invalid delivery headers.", http.StatusBadRequest)
		return
	}
	event := events[0]
	if !knownEvent(event) {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if strictjson.Validate(body) != nil {
		http.Error(w, "Invalid delivery JSON.", http.StatusBadRequest)
		return
	}
	hint, ignore, err := normalize(event, body)
	if err != nil {
		http.Error(w, "Invalid delivery selectors.", http.StatusBadRequest)
		return
	}
	if ignore {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	id := strings.ToLower(ids[0])
	digest := sha256.Sum256(body)
	h.mu.Lock()
	now := h.now()
	for key, entry := range h.deliveries {
		if entry.done && !now.Before(entry.expires) {
			delete(h.deliveries, key)
		}
	}
	if prior := h.deliveries[id]; prior != nil {
		conflict, done := prior.event != event || prior.digest != digest, prior.done
		h.mu.Unlock()
		if conflict {
			http.Error(w, "Conflicting delivery identity.", http.StatusBadRequest)
			return
		}
		if !done {
			http.Error(w, "The delivery is still being accepted.", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusAccepted)
		return
	}
	if len(h.deliveries) >= deliveryLimit {
		h.mu.Unlock()
		http.Error(w, "Delivery capacity is temporarily occupied.", http.StatusServiceUnavailable)
		return
	}
	entry := &delivery{event: event, digest: digest}
	h.deliveries[id] = entry
	h.mu.Unlock()
	err = h.callback(r.Context(), hint)
	h.mu.Lock()
	if err != nil {
		delete(h.deliveries, id)
	} else {
		entry.done = true
		entry.expires = h.now().Add(deliveryTTL)
	}
	h.mu.Unlock()
	if err != nil {
		http.Error(w, "The delivery could not be queued; retry this delivery.", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}
func validSignature(secret, body []byte, signature string) bool {
	if len(signature) != len("sha256=")+sha256.Size*2 || !strings.HasPrefix(signature, "sha256=") {
		return false
	}
	supplied, err := hex.DecodeString(strings.TrimPrefix(signature, "sha256="))
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write(body)
	return hmac.Equal(mac.Sum(nil), supplied)
}
