package main

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"text/template"
)

// badgeSVG is a flat status badge. Widths assume ~6.5px per character of 11px Verdana.
var badgeSVG = template.Must(template.New("badge").Parse(`<svg xmlns="http://www.w3.org/2000/svg" width="{{.W}}" height="20" role="img" aria-label="watchnote: {{.Label}}">
<clipPath id="r"><rect width="{{.W}}" height="20" rx="3"/></clipPath>
<g clip-path="url(#r)"><rect width="{{.LW}}" height="20" fill="#555"/><rect x="{{.LW}}" width="{{.RW}}" height="20" fill="{{.Color}}"/></g>
<g fill="#fff" text-anchor="middle" font-family="Verdana,Geneva,DejaVu Sans,sans-serif" font-size="11">
<text x="{{.LX}}" y="14">watchnote</text><text x="{{.RX}}" y="14">{{.Label}}</text></g>
</svg>`))

// badgeMarkdown shows the badge, linked to the watch's page in Watchnote.
func (a *App) badgeMarkdown(w *Watch) string {
	return fmt.Sprintf("[![Watchnote](%s/badge/%d?s=%s)](%s/items/%d)",
		a.cfg.BaseURL, w.ID, a.keys.LinkSig("badge", w.ID), a.cfg.BaseURL, w.ID)
}

// handleBadge serves a watch's done state as an image for PR descriptions. The
// signature keeps other users' watches from being enumerated.
func (a *App) handleBadge(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if !a.keys.CheckLink("badge", id, r.FormValue("s")) {
		http.Error(w, "This link isn't valid.", http.StatusForbidden)
		return
	}
	label, color := "not done", "#e05d44"
	wt, err := a.db.WatchByID(r.Context(), id)
	switch {
	case errors.Is(err, errNotFound):
		label, color = "not watched", "#9f9f9f"
	case err != nil:
		a.serverError(w, err)
		return
	case wt.Status == "done":
		label, color = "done", "#4c1"
	}
	lw, rw := 70, 10+len(label)*7
	w.Header().Set("Content-Type", "image/svg+xml")
	// GitHub's image proxy honours this, so the badge is re-fetched on every view.
	w.Header().Set("Cache-Control", "no-cache, max-age=0")
	badgeSVG.Execute(w, map[string]any{
		"W": lw + rw, "LW": lw, "RW": rw, "LX": lw / 2, "RX": lw + rw/2, "Label": label, "Color": color,
	})
}
