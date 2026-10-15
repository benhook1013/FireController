package server

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/benhook1013/FireController/internal/core"
)

func browserSessionOrigin(r *http.Request) error {
	bad := core.Fail("forbidden", "Browser session requests must use the same origin.", 403)
	sites := r.Header.Values("Sec-Fetch-Site")
	if len(sites) > 1 || len(sites) == 1 && sites[0] != "same-origin" && sites[0] != "same-site" && sites[0] != "none" {
		return bad
	}
	origins := r.Header.Values("Origin")
	if len(origins) == 0 {
		return nil
	}
	if len(origins) != 1 {
		return bad
	}
	origin, err := url.Parse(origins[0])
	scheme := "https"
	if core.BrowserSessionLocalHTTP(r) {
		scheme = "http"
	}
	if err != nil || origin.Scheme != scheme || !strings.EqualFold(origin.Host, r.Host) || origin.User != nil || origin.Path != "" || origin.RawQuery != "" || origin.Fragment != "" || origin.Opaque != "" {
		return bad
	}
	return nil
}

func (a *api) createBrowserSession(w http.ResponseWriter, r *http.Request) {
	if err := browserSessionOrigin(r); err != nil {
		core.WriteError(w, err)
		return
	}
	if _, err := core.QueryKeys(r, "validation_failed"); err != nil {
		core.WriteError(w, err)
		return
	}
	var body struct {
		SessionID string `json:"session_id"`
	}
	if err := core.ReadJSON(w, r, &body); err != nil {
		core.WriteError(w, err)
		return
	}
	response, cookie, err := a.store.CreateBrowserSession(r, body.SessionID)
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
	if err := browserSessionOrigin(r); err != nil {
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
