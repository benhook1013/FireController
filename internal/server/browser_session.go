package server

import (
	"bytes"
	"errors"
	"net/http"

	"github.com/benhook1013/FireController/internal/core"
)

// A supplied null is not an omitted approval. Its bool underlying type also
// keeps strictjson's exact-field validation applicable to the request shape.
type browserWriteApproval bool

func (approval *browserWriteApproval) UnmarshalJSON(data []byte) error {
	switch string(bytes.TrimSpace(data)) {
	case "true":
		*approval = true
	case "false":
		*approval = false
	default:
		return errors.New("browser write approval must be a boolean")
	}
	return nil
}

func (a *api) createBrowserSession(w http.ResponseWriter, r *http.Request) {
	if err := core.BrowserSessionOrigin(r, false); err != nil {
		core.WriteError(w, err)
		return
	}
	if _, err := core.QueryKeys(r, "validation_failed"); err != nil {
		core.WriteError(w, err)
		return
	}
	var body struct {
		SessionID   string               `json:"session_id"`
		AllowWrites browserWriteApproval `json:"allow_writes"`
	}
	if err := core.ReadJSON(w, r, &body); err != nil {
		core.WriteError(w, err)
		return
	}
	response, cookie, err := a.store.CreateBrowserSession(r, body.SessionID, bool(body.AllowWrites))
	if err != nil {
		core.WriteError(w, err)
		return
	}
	for _, stale := range a.store.PruneBrowserSessions(r, cookie) {
		http.SetCookie(w, stale)
	}
	http.SetCookie(w, cookie)
	core.WriteJSON(w, 200, response)
}

func (a *api) deleteBrowserSession(w http.ResponseWriter, r *http.Request) {
	if err := core.BrowserSessionOrigin(r, false); err != nil {
		core.WriteError(w, err)
		return
	}
	if _, err := core.QueryKeys(r, "validation_failed"); err != nil {
		core.WriteError(w, err)
		return
	}
	var body struct{}
	if err := core.ReadJSON(w, r, &body); err != nil {
		core.WriteError(w, err)
		return
	}
	cookie, id, err := core.ExpireBrowserSession(r)
	if err != nil {
		core.WriteError(w, err)
		return
	}
	http.SetCookie(w, cookie)
	core.WriteJSON(w, 200, struct {
		SessionID string `json:"session_id"`
		LoggedOut bool   `json:"logged_out"`
	}{id, true})
}

func (a *api) currentBrowserSession(w http.ResponseWriter, r *http.Request) {
	if _, err := core.QueryKeys(r, "validation_failed"); err != nil {
		core.WriteError(w, err)
		return
	}
	response, err := a.store.CurrentBrowserSession(r)
	if err != nil {
		core.WriteError(w, err)
		return
	}
	core.WriteJSON(w, 200, response)
}

func (a *api) renewBrowserSession(w http.ResponseWriter, r *http.Request) {
	if err := core.BrowserSessionOrigin(r, true); err != nil {
		core.WriteError(w, err)
		return
	}
	if _, err := core.QueryKeys(r, "validation_failed"); err != nil {
		core.WriteError(w, err)
		return
	}
	var body struct{}
	if err := core.ReadJSON(w, r, &body); err != nil {
		core.WriteError(w, err)
		return
	}
	response, cookie, err := a.store.RenewBrowserSession(r)
	if err != nil {
		core.WriteError(w, err)
		return
	}
	// Renewal replaces exactly the selected cookie. It neither creates a new
	// selector nor evicts sessions belonging to other tabs.
	http.SetCookie(w, cookie)
	core.WriteJSON(w, 200, response)
}
