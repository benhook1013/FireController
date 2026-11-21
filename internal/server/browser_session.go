package server

import (
	"bytes"
	"encoding/json"
	"net/http"

	"github.com/benhook1013/FireController/internal/core"
)

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
		SessionID        string          `json:"session_id"`
		ProjectSwitching json.RawMessage `json:"project_switching"`
	}
	if err := core.ReadJSON(w, r, &body); err != nil {
		core.WriteError(w, err)
		return
	}
	switching := false
	switch string(bytes.TrimSpace(body.ProjectSwitching)) {
	case "", "false":
	case "true":
		switching = true
	default:
		core.WriteError(w, core.Fail("validation_failed", "Project switching must be a boolean when supplied.", 400))
		return
	}
	response, cookie, err := a.store.CreateBrowserSessionWithProjectSwitching(r, body.SessionID, switching)
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

func (a *api) switchBrowserSession(w http.ResponseWriter, r *http.Request) {
	if err := core.BrowserSessionOrigin(r, true); err != nil {
		core.WriteError(w, err)
		return
	}
	if _, err := core.QueryKeys(r, "validation_failed"); err != nil {
		core.WriteError(w, err)
		return
	}
	var body struct {
		ProjectID string `json:"project_id"`
		SessionID string `json:"session_id"`
	}
	if err := core.ReadJSON(w, r, &body); err != nil {
		core.WriteError(w, err)
		return
	}
	response, cookie, err := a.store.SwitchBrowserSession(r, body.ProjectID, body.SessionID)
	if err != nil {
		core.WriteError(w, err)
		return
	}
	stale, err := a.store.PruneBrowserSwitchSessions(r, cookie)
	if err != nil {
		core.WriteError(w, err)
		return
	}
	for _, expired := range stale {
		http.SetCookie(w, expired)
	}
	http.SetCookie(w, cookie)
	core.WriteJSON(w, 200, response)
}
