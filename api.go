package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// The JSON API backs the GitHub userscript. It authenticates with a personal
// token generated in Settings, since the userscript runs on github.com and
// has no Watchnote cookie.

// apiUser authenticates a request by its personal token (the userscript's and MCP's).
func (a *App) apiUser(r *http.Request) (*User, error) {
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || tok == "" {
		return nil, errors.New("missing token; generate one in Watchnote settings")
	}
	u, err := a.db.UserByAPITokenHash(r.Context(), hashToken(tok))
	if err != nil {
		return nil, errors.New("invalid token; generate a new one in Watchnote settings")
	}
	return u, nil
}

func (a *App) withAPIUser(h userHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u, err := a.apiUser(r)
		if err != nil {
			apiError(w, http.StatusUnauthorized, err.Error())
			return
		}
		h(w, r, u)
	}
}

func apiError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

type apiWatch struct {
	Watching bool   `json:"watching"`
	ID       int64  `json:"id,omitempty"`
	URL      string `json:"url,omitempty"`
	Status   string `json:"status,omitempty"`
	Note     string `json:"note,omitempty"`
}

func (a *App) apiWatchBody(wt *Watch) apiWatch {
	return apiWatch{Watching: true, ID: wt.ID, URL: fmt.Sprintf("%s/items/%d", a.cfg.BaseURL, wt.ID),
		Status: wt.Status, Note: wt.Note}
}

func (a *App) apiLookup(w http.ResponseWriter, r *http.Request, u *User) {
	ref, ok := parseRef(r.URL.Query().Get("url"))
	if !ok {
		apiError(w, 400, "not a GitHub issue or PR URL")
		return
	}
	it, err := a.db.ItemByRef(r.Context(), ref.Owner, ref.Repo, ref.Number)
	if err == nil {
		if wt, err := a.db.UserWatchForItem(r.Context(), u.ID, it.ID); err == nil {
			writeJSON(w, 200, a.apiWatchBody(wt))
			return
		}
	}
	writeJSON(w, 200, apiWatch{Watching: false})
}

func (a *App) apiCreate(w http.ResponseWriter, r *http.Request, u *User) {
	var body struct {
		URL    string `json:"url"`
		Note   string `json:"note"`
		Preset string `json:"preset"` // everything | status
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&body); err != nil {
		apiError(w, 400, "bad JSON")
		return
	}
	ref, ok := parseRef(body.URL)
	note := cleanNote(body.Note)
	switch {
	case !ok:
		apiError(w, 400, "not a GitHub issue or PR URL")
		return
	case note == "":
		apiError(w, 400, "add a note about why you're watching this")
		return
	}
	filter := u.DefaultFilter
	switch body.Preset {
	case "everything":
		filter = filterEverything
	case "status":
		filter = filterStatusOnly
	}
	wt, created, err := a.AddWatch(r.Context(), u, ref, note, filter, u.DefaultDelivery)
	if err != nil {
		code := 502
		if errors.Is(err, errGHNotFound) {
			code = 404
		}
		apiError(w, code, previewError(err, u))
		return
	}
	code := 200
	if created {
		code = 201
	}
	writeJSON(w, code, a.apiWatchBody(wt))
}

func (a *App) handleUserscript(w http.ResponseWriter, r *http.Request) {
	src, err := assets.ReadFile("static/watchnote.user.js")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	host := strings.TrimPrefix(strings.TrimPrefix(a.cfg.BaseURL, "https://"), "http://")
	host, _, _ = strings.Cut(host, "/")
	host, _, _ = strings.Cut(host, ":")
	out := strings.NewReplacer("__BASE_URL__", a.cfg.BaseURL, "__HOST__", host).Replace(string(src))
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	w.Write([]byte(out))
}
