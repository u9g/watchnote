package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type pageData struct {
	Title   string
	User    *User
	CSRF    string
	BaseURL string

	// watchlist + detail
	Counts   map[string]int
	Tab      string
	Q        string
	Watches  []*Watch
	Selected *Watch
	Events   []*Event
	NewSince int64

	// new watch
	URL      string
	Preview  *Item
	Existing *Watch
	Note     string
	Filter   string
	Delivery string

	// settings
	APIToken string
	Saved    bool

	// email actions
	Action      string
	ActionTitle string
	ActionDesc  string
	WatchID     int64
	Sig         string
	ActionDone  bool
	ActionWatch *Watch

	// landing
	Next     string
	DevLogin bool
	GoogleOn bool
	Error    string
}

const sessionCookie = "wn_session"

func (a *App) routes() http.Handler {
	mux := http.NewServeMux()
	static, _ := fs.Sub(assets, "static")
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(static)))
	mux.HandleFunc("GET /sw.js", a.serveStatic("sw.js", "text/javascript"))
	mux.HandleFunc("GET /manifest.webmanifest", a.serveStatic("manifest.webmanifest", "application/manifest+json"))
	for _, p := range []string{"/icon-32.png", "/icon-192.png", "/icon-512.png"} {
		mux.HandleFunc("GET "+p, a.handleIcon)
	}
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		if err := a.db.PingContext(r.Context()); err != nil {
			http.Error(w, "db: "+err.Error(), 503)
			return
		}
		w.Write([]byte("ok"))
	})

	mux.HandleFunc("GET /{$}", a.handleLanding)
	mux.HandleFunc("GET /auth/google", a.handleGoogleStart)
	mux.HandleFunc("GET /auth/google/callback", a.handleGoogleCallback)
	mux.HandleFunc("POST /auth/dev", a.handleDevLogin)
	mux.HandleFunc("POST /logout", a.handleLogout)

	mux.HandleFunc("GET /items", a.withUser(a.handleList))
	mux.HandleFunc("GET /items/new", a.withUser(a.handleNewForm))
	mux.HandleFunc("POST /items", a.withUser(a.handleCreate))
	mux.HandleFunc("GET /items/{id}", a.withUser(a.handleDetail))
	mux.HandleFunc("POST /items/{id}/note", a.withUser(a.handleNote))
	mux.HandleFunc("POST /items/{id}/prefs", a.withUser(a.handlePrefs))
	mux.HandleFunc("POST /items/{id}/status", a.withUser(a.handleStatus))
	mux.HandleFunc("POST /items/{id}/delete", a.withUser(a.handleDelete))
	mux.HandleFunc("GET /share", a.withUser(a.handleShare))

	mux.HandleFunc("GET /settings", a.withUser(a.handleSettings))
	mux.HandleFunc("POST /settings", a.withUser(a.handleSettingsSave))
	mux.HandleFunc("POST /settings/tz", a.withUser(a.handleSetTZ))
	mux.HandleFunc("POST /settings/github", a.withUser(a.handleGitHubToken))
	mux.HandleFunc("POST /settings/github/remove", a.withUser(a.handleGitHubRemove))
	mux.HandleFunc("POST /settings/api-token", a.withUser(a.handleAPIToken))

	mux.HandleFunc("GET /e/{action}", a.handleEmailAction)
	mux.HandleFunc("POST /e/{action}", a.handleEmailAction)

	mux.HandleFunc("GET /api/watch", a.withAPIUser(a.apiLookup))
	mux.HandleFunc("POST /api/watch", a.withAPIUser(a.apiCreate))
	mux.HandleFunc("GET /watchnote.user.js", a.handleUserscript)
	mux.Handle("/mcp", a.mcpHandler())

	return securityHeaders(mux)
}

func securityHeaders(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data: https:; "+
			"style-src 'self'; script-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "same-origin")
		h.ServeHTTP(w, r)
	})
}

func (a *App) serveStatic(name, ctype string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		b, err := assets.ReadFile("static/" + name)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", ctype)
		w.Write(b)
	}
}

// ---- sessions ----

func (a *App) secureCookies() bool { return strings.HasPrefix(a.cfg.BaseURL, "https://") }

func (a *App) setCookie(w http.ResponseWriter, name, value string, maxAge time.Duration) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: value, Path: "/", MaxAge: int(maxAge.Seconds()),
		HttpOnly: true, Secure: a.secureCookies(), SameSite: http.SameSiteLaxMode})
}

func (a *App) currentUser(r *http.Request) *User {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return nil
	}
	uid, ok := a.keys.ParseSession(c.Value, a.now())
	if !ok {
		return nil
	}
	u, err := a.db.UserByID(r.Context(), uid)
	if err != nil {
		return nil
	}
	return u
}

func (a *App) startSession(w http.ResponseWriter, u *User) {
	a.setCookie(w, sessionCookie, a.keys.SessionValue(u.ID, a.now()), sessionTTL)
}

type userHandler func(w http.ResponseWriter, r *http.Request, u *User)

// withUser requires a signed-in user and, for POSTs, a valid CSRF token.
func (a *App) withUser(h userHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u := a.currentUser(r)
		if u == nil {
			if r.Method == http.MethodGet {
				http.Redirect(w, r, "/?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusSeeOther)
			} else {
				http.Error(w, "signed out", http.StatusUnauthorized)
			}
			return
		}
		if r.Method == http.MethodPost && !macEqual(r.FormValue("csrf"), a.keys.CSRF(u.ID)) {
			http.Error(w, "invalid form token; reload the page and try again", http.StatusForbidden)
			return
		}
		h(w, r, u)
	}
}

func (a *App) page(u *User, title string) *pageData {
	d := &pageData{Title: title, User: u, BaseURL: a.cfg.BaseURL}
	if u != nil {
		d.CSRF = a.keys.CSRF(u.ID)
		d.Filter, d.Delivery = u.DefaultFilter, u.DefaultDelivery
	}
	return d
}

func safeNext(next string) string {
	if strings.HasPrefix(next, "/") && !strings.HasPrefix(next, "//") && !strings.HasPrefix(next, `/\`) {
		return next
	}
	return "/items"
}

// ---- landing + auth ----

func (a *App) handleLanding(w http.ResponseWriter, r *http.Request) {
	if a.currentUser(r) != nil {
		http.Redirect(w, r, "/items", http.StatusSeeOther)
		return
	}
	d := a.page(nil, "Watchnote")
	d.Next = r.URL.Query().Get("next")
	d.DevLogin = a.cfg.DevLogin
	d.GoogleOn = a.cfg.GoogleClientID != ""
	d.Error = r.URL.Query().Get("error")
	a.render(w, 200, "landing", d)
}

func (a *App) handleGoogleStart(w http.ResponseWriter, r *http.Request) {
	state := randomToken(18)
	a.setCookie(w, "wn_oauth", state+"|"+safeNext(r.URL.Query().Get("next")), 10*time.Minute)
	q := url.Values{
		"client_id":     {a.cfg.GoogleClientID},
		"redirect_uri":  {a.cfg.BaseURL + "/auth/google/callback"},
		"response_type": {"code"},
		"scope":         {"openid email profile"},
		"state":         {state},
		"prompt":        {"select_account"},
	}
	http.Redirect(w, r, "https://accounts.google.com/o/oauth2/v2/auth?"+q.Encode(), http.StatusSeeOther)
}

func (a *App) handleGoogleCallback(w http.ResponseWriter, r *http.Request) {
	fail := func(msg string, err error) {
		a.log.Warn("google sign-in failed", "msg", msg, "err", err)
		http.Redirect(w, r, "/?error="+url.QueryEscape(msg), http.StatusSeeOther)
	}
	c, err := r.Cookie("wn_oauth")
	if err != nil {
		fail("Sign-in expired, please try again.", err)
		return
	}
	a.setCookie(w, "wn_oauth", "", -time.Second)
	state, next, _ := strings.Cut(c.Value, "|")
	if r.URL.Query().Get("state") == "" || !macEqual(r.URL.Query().Get("state"), state) {
		fail("Sign-in expired, please try again.", nil)
		return
	}
	if e := r.URL.Query().Get("error"); e != "" {
		fail("Google sign-in was cancelled.", errors.New(e))
		return
	}
	info, err := a.googleUserInfo(r.Context(), r.URL.Query().Get("code"))
	if err != nil {
		fail("Couldn't finish signing in with Google.", err)
		return
	}
	if !info.EmailVerified || info.Email == "" {
		fail("Your Google account's email address isn't verified.", nil)
		return
	}
	u, err := a.db.UpsertGoogleUser(r.Context(), info.Sub, info.Email, info.Name, info.Picture)
	if err != nil {
		fail("Couldn't finish signing in.", err)
		return
	}
	a.startSession(w, u)
	http.Redirect(w, r, safeNext(next), http.StatusSeeOther)
}

type googleInfo struct {
	Sub           string `json:"sub"`
	Email         string `json:"email"`
	EmailVerified bool   `json:"email_verified"`
	Name          string `json:"name"`
	Picture       string `json:"picture"`
}

func (a *App) googleUserInfo(ctx context.Context, code string) (*googleInfo, error) {
	form := url.Values{
		"code":          {code},
		"client_id":     {a.cfg.GoogleClientID},
		"client_secret": {a.cfg.GoogleClientSecret},
		"redirect_uri":  {a.cfg.BaseURL + "/auth/google/callback"},
		"grant_type":    {"authorization_code"},
	}
	req, _ := http.NewRequestWithContext(ctx, "POST", "https://oauth2.googleapis.com/token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := a.gh.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var tok struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tok); err != nil || tok.AccessToken == "" {
		return nil, fmt.Errorf("token exchange: %v %s", err, tok.Error)
	}
	req, _ = http.NewRequestWithContext(ctx, "GET", "https://openidconnect.googleapis.com/v1/userinfo", nil)
	req.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	resp, err = a.gh.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("userinfo: %s", resp.Status)
	}
	var info googleInfo
	return &info, json.NewDecoder(resp.Body).Decode(&info)
}

// handleDevLogin signs in as any email. Only for local development (DEV_LOGIN=true).
func (a *App) handleDevLogin(w http.ResponseWriter, r *http.Request) {
	if !a.cfg.DevLogin {
		http.NotFound(w, r)
		return
	}
	email := strings.TrimSpace(r.FormValue("email"))
	if !strings.Contains(email, "@") {
		http.Redirect(w, r, "/?error=Enter+an+email", http.StatusSeeOther)
		return
	}
	u, err := a.db.UpsertGoogleUser(r.Context(), "dev:"+email, email, strings.Split(email, "@")[0], "")
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	a.startSession(w, u)
	http.Redirect(w, r, safeNext(r.FormValue("next")), http.StatusSeeOther)
}

func (a *App) handleLogout(w http.ResponseWriter, r *http.Request) {
	a.setCookie(w, sessionCookie, "", -time.Second)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// ---- watchlist ----

func validTab(t string) string {
	if t == "done" || t == "muted" {
		return t
	}
	return "active"
}

func (a *App) listData(r *http.Request, u *User, tab string) (*pageData, error) {
	d := a.page(u, "Watchnote")
	d.Tab = tab
	d.Q = r.URL.Query().Get("q")
	var err error
	if d.Counts, err = a.db.CountWatches(r.Context(), u.ID); err != nil {
		return nil, err
	}
	if d.Watches, err = a.db.ListWatches(r.Context(), u.ID, tab, d.Q); err != nil {
		return nil, err
	}
	return d, nil
}

func (a *App) handleList(w http.ResponseWriter, r *http.Request, u *User) {
	d, err := a.listData(r, u, validTab(r.URL.Query().Get("tab")))
	if err != nil {
		a.serverError(w, err)
		return
	}
	a.render(w, 200, "items", d)
}

func (a *App) watchFromPath(w http.ResponseWriter, r *http.Request, u *User) *Watch {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	wt, err := a.db.UserWatch(r.Context(), u.ID, id)
	if err != nil {
		http.NotFound(w, r)
		return nil
	}
	return wt
}

func (a *App) handleDetail(w http.ResponseWriter, r *http.Request, u *User) {
	wt := a.watchFromPath(w, r, u)
	if wt == nil {
		return
	}
	d, err := a.listData(r, u, wt.Status)
	if err != nil {
		a.serverError(w, err)
		return
	}
	d.Title = wt.Item.Ref() + " · Watchnote"
	d.Selected = wt
	d.NewSince = wt.ViewedEventID
	if d.Events, err = a.db.RecentEvents(r.Context(), wt.ItemID, 100); err != nil {
		a.serverError(w, err)
		return
	}
	if len(d.Events) > 0 {
		a.db.SetWatchViewed(r.Context(), wt.ID, d.Events[0].ID)
	}
	a.render(w, 200, "items", d)
}

func (a *App) handleNewForm(w http.ResponseWriter, r *http.Request, u *User) {
	d := a.page(u, "Watch · Watchnote")
	d.URL = strings.TrimSpace(r.URL.Query().Get("url"))
	if d.URL != "" {
		ref, ok := parseRef(d.URL)
		if !ok {
			d.Error = "That doesn't look like a GitHub issue or pull request link."
		} else {
			d.URL = fmt.Sprintf("https://github.com/%s/%s/issues/%d", ref.Owner, ref.Repo, ref.Number)
			if it, err := a.db.ItemByRef(r.Context(), ref.Owner, ref.Repo, ref.Number); err == nil {
				if ex, err := a.db.UserWatchForItem(r.Context(), u.ID, it.ID); err == nil {
					http.Redirect(w, r, fmt.Sprintf("/items/%d", ex.ID), http.StatusSeeOther)
					return
				}
			}
			p, err := a.Preview(r.Context(), u, ref)
			d.Preview = p
			if err != nil {
				d.Error = previewError(err, u)
			} else {
				d.URL = d.Preview.HTMLURL
			}
		}
	}
	a.render(w, 200, "new", d)
}

func previewError(err error, u *User) string {
	switch {
	case errors.Is(err, errGHNotFound) && u.GitHubToken == nil:
		return "GitHub says that doesn't exist. If it's in a private repo, add a GitHub token in Settings first."
	case errors.Is(err, errGHNotFound):
		return "GitHub says that doesn't exist, or your saved GitHub token can't see it."
	case errors.Is(err, errGHRateLimited):
		return "We've hit GitHub's rate limit. Try again in a few minutes."
	default:
		return "Couldn't reach GitHub: " + err.Error()
	}
}

func formFilter(r *http.Request) string {
	var picked []string
	for _, c := range categories {
		for _, v := range r.Form["cat"] {
			if v == c.Key {
				picked = append(picked, c.Key)
				break
			}
		}
	}
	return strings.Join(picked, ",")
}

func formDelivery(v string) string {
	if v == "digest" {
		return "digest"
	}
	return "instant"
}

const maxNote = 2000

func cleanNote(s string) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\r\n", "\n"))
	if r := []rune(s); len(r) > maxNote {
		s = string(r[:maxNote])
	}
	return s
}

func (a *App) handleCreate(w http.ResponseWriter, r *http.Request, u *User) {
	d := a.page(u, "Watch · Watchnote")
	d.URL = r.FormValue("url")
	d.Note = cleanNote(r.FormValue("note"))
	d.Filter = formFilter(r)
	d.Delivery = formDelivery(r.FormValue("delivery"))
	ref, ok := parseRef(d.URL)
	switch {
	case !ok:
		d.Error = "That doesn't look like a GitHub issue or pull request link."
	case d.Note == "":
		d.Error = "Add a note about why you're watching this. It goes at the top of every email."
	case d.Filter == "":
		d.Error = "Pick at least one kind of update to be emailed about."
	}
	if d.Error == "" {
		wt, _, err := a.AddWatch(r.Context(), u, ref, d.Note, d.Filter, d.Delivery)
		if err == nil {
			http.Redirect(w, r, fmt.Sprintf("/items/%d", wt.ID), http.StatusSeeOther)
			return
		}
		d.Error = previewError(err, u)
	}
	if ok {
		d.Preview, _ = a.Preview(r.Context(), u, ref)
	}
	a.render(w, 422, "new", d)
}

func (a *App) handleNote(w http.ResponseWriter, r *http.Request, u *User) {
	wt := a.watchFromPath(w, r, u)
	if wt == nil {
		return
	}
	note := cleanNote(r.FormValue("note"))
	if note == "" {
		http.Error(w, "The note can't be empty.", 422)
		return
	}
	if err := a.db.SetWatchNote(r.Context(), wt.ID, note); err != nil {
		a.serverError(w, err)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/items/%d", wt.ID), http.StatusSeeOther)
}

func (a *App) handlePrefs(w http.ResponseWriter, r *http.Request, u *User) {
	wt := a.watchFromPath(w, r, u)
	if wt == nil {
		return
	}
	filter := formFilter(r)
	if filter == "" {
		http.Error(w, "Pick at least one kind of update.", 422)
		return
	}
	if err := a.db.SetWatchPrefs(r.Context(), wt.ID, filter, formDelivery(r.FormValue("delivery"))); err != nil {
		a.serverError(w, err)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/items/%d", wt.ID), http.StatusSeeOther)
}

func (a *App) handleStatus(w http.ResponseWriter, r *http.Request, u *User) {
	wt := a.watchFromPath(w, r, u)
	if wt == nil {
		return
	}
	status := validTab(r.FormValue("status"))
	if err := a.db.SetWatchStatus(r.Context(), wt.ID, status); err != nil {
		a.serverError(w, err)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/items/%d", wt.ID), http.StatusSeeOther)
}

func (a *App) handleDelete(w http.ResponseWriter, r *http.Request, u *User) {
	wt := a.watchFromPath(w, r, u)
	if wt == nil {
		return
	}
	if err := a.db.DeleteWatch(r.Context(), wt.ID); err != nil {
		a.serverError(w, err)
		return
	}
	http.Redirect(w, r, "/items?tab="+wt.Status, http.StatusSeeOther)
}

// handleShare receives links from the mobile share sheet (PWA share_target).
func (a *App) handleShare(w http.ResponseWriter, r *http.Request, u *User) {
	q := r.URL.Query()
	for _, s := range []string{q.Get("url"), q.Get("text"), q.Get("title")} {
		if ref, ok := parseRef(s); ok {
			target := fmt.Sprintf("https://github.com/%s/%s/issues/%d", ref.Owner, ref.Repo, ref.Number)
			http.Redirect(w, r, "/items/new?url="+url.QueryEscape(target), http.StatusSeeOther)
			return
		}
	}
	http.Redirect(w, r, "/items/new?url="+url.QueryEscape(q.Get("url")+q.Get("text")), http.StatusSeeOther)
}

// ---- settings ----

func (a *App) handleSettings(w http.ResponseWriter, r *http.Request, u *User) {
	d := a.page(u, "Settings · Watchnote")
	d.Saved = r.URL.Query().Get("saved") == "1"
	d.Error = r.URL.Query().Get("error")
	a.render(w, 200, "settings", d)
}

func parseClock(s string) (sql.NullInt64, bool) {
	t, err := time.Parse("15:04", s)
	if err != nil {
		return sql.NullInt64{}, false
	}
	return sql.NullInt64{Int64: int64(t.Hour()*60 + t.Minute()), Valid: true}, true
}

func (a *App) handleSettingsSave(w http.ResponseWriter, r *http.Request, u *User) {
	s := UserSettings{
		TZ:              strings.TrimSpace(r.FormValue("tz")),
		DefaultFilter:   formFilter(r),
		DefaultDelivery: formDelivery(r.FormValue("delivery")),
	}
	fail := func(msg string) { http.Redirect(w, r, "/settings?error="+url.QueryEscape(msg), http.StatusSeeOther) }
	if _, err := time.LoadLocation(s.TZ); err != nil || s.TZ == "" {
		fail("Unknown timezone " + s.TZ)
		return
	}
	if s.DefaultFilter == "" {
		fail("Pick at least one kind of update.")
		return
	}
	h, err := strconv.Atoi(r.FormValue("digest_hour"))
	if err != nil || h < 0 || h > 23 {
		fail("Pick a digest hour.")
		return
	}
	s.DigestHour = h
	if r.FormValue("quiet") == "on" {
		var ok1, ok2 bool
		s.QuietStart, ok1 = parseClock(r.FormValue("quiet_start"))
		s.QuietEnd, ok2 = parseClock(r.FormValue("quiet_end"))
		if !ok1 || !ok2 {
			fail("Quiet hours need a start and end time.")
			return
		}
	}
	if err := a.db.UpdateUserSettings(r.Context(), u.ID, s); err != nil {
		a.serverError(w, err)
		return
	}
	http.Redirect(w, r, "/settings?saved=1", http.StatusSeeOther)
}

// handleSetTZ is called once by the browser to record its timezone.
func (a *App) handleSetTZ(w http.ResponseWriter, r *http.Request, u *User) {
	tz := r.FormValue("tz")
	if _, err := time.LoadLocation(tz); u.TZ == "" && tz != "" && err == nil {
		a.db.SetUserTZ(r.Context(), u.ID, tz)
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *App) handleGitHubToken(w http.ResponseWriter, r *http.Request, u *User) {
	tok := strings.TrimSpace(r.FormValue("token"))
	login, err := a.gh.Viewer(r.Context(), tok)
	if tok == "" || err != nil {
		http.Redirect(w, r, "/settings?error="+url.QueryEscape("GitHub didn't accept that token."), http.StatusSeeOther)
		return
	}
	sealed, err := a.keys.Seal(tok)
	if err == nil {
		err = a.db.SetGitHubToken(r.Context(), u.ID, sealed, login)
	}
	if err != nil {
		a.serverError(w, err)
		return
	}
	http.Redirect(w, r, "/settings?saved=1", http.StatusSeeOther)
}

func (a *App) handleGitHubRemove(w http.ResponseWriter, r *http.Request, u *User) {
	if err := a.db.SetGitHubToken(r.Context(), u.ID, nil, ""); err != nil {
		a.serverError(w, err)
		return
	}
	http.Redirect(w, r, "/settings?saved=1", http.StatusSeeOther)
}

func (a *App) handleAPIToken(w http.ResponseWriter, r *http.Request, u *User) {
	tok := "wn_" + randomToken(24)
	if err := a.db.SetAPITokenHash(r.Context(), u.ID, hashToken(tok)); err != nil {
		a.serverError(w, err)
		return
	}
	u.HasAPIToken = true
	d := a.page(u, "Settings · Watchnote")
	d.APIToken = tok
	a.render(w, 200, "settings", d)
}

// ---- signed email actions (no login needed) ----

var emailActions = map[string][2]string{
	"mute":       {"Mute this item", "You'll stop getting emails about it. It stays in your Muted list and you can unmute it any time."},
	"statusonly": {"Status changes only", "You'll only be emailed when it's merged, closed or reopened."},
	"stop":       {"Stop watching", "It'll be removed from Watchnote along with your note."},
	"done":       {"Mark as done", "It moves to your Done list. You'll hear about it again only if it's reopened."},
}

// handleEmailAction shows a confirmation page on GET (so link scanners that
// prefetch emails can't trigger anything) and acts on POST. POST also serves
// RFC 8058 one-click unsubscribe.
func (a *App) handleEmailAction(w http.ResponseWriter, r *http.Request) {
	action := r.PathValue("action")
	text, known := emailActions[action]
	id, _ := strconv.ParseInt(r.FormValue("w"), 10, 64)
	sig := r.FormValue("s")
	if !known || !a.keys.CheckLink(action, id, sig) {
		http.Error(w, "This link isn't valid.", http.StatusForbidden)
		return
	}
	d := a.page(a.currentUser(r), text[0]+" · Watchnote")
	d.Action, d.ActionTitle, d.ActionDesc, d.WatchID, d.Sig = action, text[0], text[1], id, sig
	wt, err := a.db.WatchByID(r.Context(), id)
	if errors.Is(err, errNotFound) {
		d.ActionDone = true
		d.ActionDesc = "You're no longer watching this item."
		a.render(w, 200, "action", d)
		return
	} else if err != nil {
		a.serverError(w, err)
		return
	}
	d.ActionWatch = wt
	if r.Method == http.MethodPost {
		ctx := r.Context()
		switch action {
		case "mute":
			err = a.db.SetWatchStatus(ctx, id, "muted")
		case "done":
			err = a.db.SetWatchStatus(ctx, id, "done")
		case "statusonly":
			err = a.db.SetWatchPrefs(ctx, id, filterStatusOnly, wt.Delivery)
		case "stop":
			err = a.db.DeleteWatch(ctx, id)
		}
		if err != nil {
			a.serverError(w, err)
			return
		}
		d.ActionDone = true
	}
	a.render(w, 200, "action", d)
}

func (a *App) serverError(w http.ResponseWriter, err error) {
	a.log.Error("request failed", "err", err)
	http.Error(w, "Something went wrong.", 500)
}
