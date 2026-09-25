package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeGitHub serves one issue and its timeline, with an ETag that changes
// whenever the test adds events.
type fakeGitHub struct {
	mu       sync.Mutex
	issue    map[string]any
	timeline []map[string]any
	version  int
	calls    map[string]int
	release  string          // latest release tag, "" for none
	inTag    map[string]bool // tags containing the merge commit

	// Repos for /user/repos; me/app's default branch is at sha with files code.
	repos []map[string]any
	sha   string
	code  map[string]string
}

func (f *fakeGitHub) add(ev map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.timeline = append(f.timeline, ev)
	f.version++
}

func (f *fakeGitHub) setState(state string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.issue["state"] = state
	f.version++
}

func (f *fakeGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	path := strings.ToLower(r.URL.Path) // GitHub is case-insensitive about owner/repo
	f.calls[path]++
	switch path {
	case "/repos/octo/hello/issues/7", "/repos/octo/hello/pulls/7":
		etag := fmt.Sprintf(`"v%d"`, f.version)
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(304)
			return
		}
		w.Header().Set("ETag", etag)
		json.NewEncoder(w).Encode(f.issue)
	case "/repos/octo/hello/issues/7/timeline":
		// 2 events per page to exercise pagination.
		page := 1
		fmt.Sscan(r.URL.Query().Get("page"), &page)
		lo, hi := (page-1)*2, page*2
		if lo > len(f.timeline) {
			lo = len(f.timeline)
		}
		if hi > len(f.timeline) {
			hi = len(f.timeline)
		}
		if hi < len(f.timeline) {
			w.Header().Set("Link", fmt.Sprintf(`<x?page=%d>; rel="next"`, page+1))
		}
		json.NewEncoder(w).Encode(f.timeline[lo:hi])
	case "/repos/octo/hello/releases/latest":
		etag := `"` + f.release + `"`
		switch {
		case f.release == "":
			w.WriteHeader(404)
		case r.Header.Get("If-None-Match") == etag:
			w.WriteHeader(304)
		default:
			w.Header().Set("ETag", etag)
			json.NewEncoder(w).Encode(map[string]string{"tag_name": f.release,
				"html_url": "https://github.com/octo/hello/releases/tag/" + f.release, "published_at": "2026-09-25T10:00:00Z"})
		}
	case "/repos/octo/hello/compare/" + strings.ToLower(f.release) + "...abc123":
		status := "ahead"
		if f.inTag[f.release] {
			status = "behind"
		}
		json.NewEncoder(w).Encode(map[string]string{"status": status})
	case "/user":
		json.NewEncoder(w).Encode(map[string]string{"login": "octocat"})
	case "/user/repos":
		json.NewEncoder(w).Encode(f.repos)
	case "/repos/me/app/git/ref/heads/main":
		json.NewEncoder(w).Encode(map[string]any{"object": map[string]string{"sha": f.sha}})
	case "/repos/me/app/tarball/" + f.sha:
		gz := gzip.NewWriter(w)
		tw := tar.NewWriter(gz)
		for name, body := range f.code {
			tw.WriteHeader(&tar.Header{Name: "me-app-" + f.sha[:7] + "/" + name, Mode: 0o644, Size: int64(len(body))})
			tw.Write([]byte(body))
		}
		tw.Close()
		gz.Close()
	default:
		w.WriteHeader(404)
		w.Write([]byte(`{"message":"Not Found"}`))
	}
}

func comment(id int, who, body string) map[string]any {
	return map[string]any{"event": "commented", "id": id, "user": map[string]string{"login": who}, "body": body,
		"html_url": fmt.Sprintf("https://github.com/octo/hello/issues/7#issuecomment-%d", id), "created_at": "2026-09-20T10:00:00Z"}
}

func simple(event string, id int, who string) map[string]any {
	return map[string]any{"event": event, "id": id, "actor": map[string]string{"login": who}, "created_at": "2026-09-20T10:00:00Z"}
}

type harness struct {
	t    *testing.T
	app  *App
	gh   *fakeGitHub
	mail *logSender
	now  time.Time
	user *User
	srv  *httptest.Server
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	gh := &fakeGitHub{calls: map[string]int{}, issue: map[string]any{
		"title": "Flaky shutdown", "state": "open", "html_url": "https://github.com/octo/hello/issues/7",
		"user": map[string]string{"login": "someone"}, "comments": 3, "updated_at": "2026-09-20T10:00:00Z",
	}}
	ghSrv := httptest.NewServer(gh)
	t.Cleanup(ghSrv.Close)

	db, err := openDB(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	mail := &logSender{}
	cfg := Config{BaseURL: "https://watchnote.test", SecretKey: strings.Repeat("k", 32), GitHubAPIURL: ghSrv.URL,
		MailFrom: "Watchnote <notify@watchnote.test>"}
	app, err := newApp(cfg, db, mail)
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, app: app, gh: gh, mail: mail, now: time.Date(2026, 9, 24, 15, 0, 0, 0, time.UTC)}
	app.now = func() time.Time { return h.now }
	h.user, err = db.UpsertGoogleUser(context.Background(), "sub-1", "me@example.com", "Me", "")
	if err != nil {
		t.Fatal(err)
	}
	h.srv = httptest.NewServer(app.routes())
	t.Cleanup(h.srv.Close)
	return h
}

func (h *harness) ctx() context.Context { return context.Background() }

// tick advances the clock, polls everything due, and runs the mailer.
func (h *harness) tick(d time.Duration) {
	h.t.Helper()
	h.now = h.now.Add(d)
	h.app.db.Exec(`UPDATE items SET next_poll_at = 0`)
	if err := h.app.pollDue(h.ctx()); err != nil {
		h.t.Fatal(err)
	}
	if err := h.app.runMailer(h.ctx()); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) addWatch(filter string) *Watch {
	h.t.Helper()
	w, created, err := h.app.AddWatch(h.ctx(), h.user, Ref{"Octo", "hello", 7}, "Remove the sleep in tests/shutdown.rs", filter, "instant")
	if err != nil || !created {
		h.t.Fatalf("AddWatch: %v created=%v", err, created)
	}
	return w
}

func TestActivityEmailFlow(t *testing.T) {
	h := newHarness(t)
	h.gh.add(comment(1, "alice", "old comment"))
	h.gh.add(simple("labeled", 2, "bob"))
	h.gh.add(simple("subscribed", 3, "bob")) // ignored noise
	w := h.addWatch(filterEverything)

	// Existing history is not emailed.
	h.tick(time.Hour)
	if len(h.mail.Sent) != 0 {
		t.Fatalf("history was emailed: %v", h.mail.Sent[0].Subject)
	}

	h.gh.add(comment(4, "carol", "Rebased on main, should merge soon."))
	h.gh.add(simple("closed", 5, "carol"))
	h.gh.setState("closed")
	h.tick(time.Second)
	if len(h.mail.Sent) != 0 {
		t.Fatal("sent before the batch window settled")
	}
	h.tick(2 * time.Minute)
	if len(h.mail.Sent) != 1 {
		t.Fatalf("want 1 email, got %d", len(h.mail.Sent))
	}
	m := h.mail.Sent[0]
	if want := "[octo/hello#7] Closed (+1 more)"; m.Subject != want {
		t.Errorf("subject = %q, want %q", m.Subject, want)
	}
	for _, s := range []string{"Remove the sleep in tests/shutdown.rs", "carol commented", "Is that what you were waiting for?", "/e/done?w="} {
		if !strings.Contains(m.Text, s) {
			t.Errorf("email text missing %q:\n%s", s, m.Text)
		}
	}
	if !strings.Contains(m.HTML, "Why you saved this:") {
		t.Error("HTML email missing note block")
	}
	if m.Headers["References"] != fmt.Sprintf("<watch-%d@watchnote.test>", w.ID) {
		t.Errorf("References = %q", m.Headers["References"])
	}

	// Nothing new: nothing sent, and the unchanged ETag means no timeline fetch.
	before := h.gh.calls["/repos/octo/hello/issues/7/timeline"]
	h.tick(5 * time.Minute)
	if len(h.mail.Sent) != 1 {
		t.Fatal("re-sent without new events")
	}
	if after := h.gh.calls["/repos/octo/hello/issues/7/timeline"]; after != before {
		t.Errorf("timeline refetched on 304 (%d -> %d)", before, after)
	}
}

func TestReleasedEmail(t *testing.T) {
	h := newHarness(t)
	h.gh.issue["pull_request"] = map[string]any{"merged_at": nil}
	h.addWatch(filterStatusOnly)
	h.tick(time.Hour)

	h.gh.mu.Lock()
	h.gh.issue["state"], h.gh.issue["merged_at"], h.gh.issue["merge_commit_sha"] = "closed", "2026-09-24T16:00:00Z", "abc123"
	h.gh.release, h.gh.inTag = "v1.0", map[string]bool{"v1.1": true}
	h.gh.version++
	h.gh.mu.Unlock()
	h.gh.add(simple("merged", 1, "carol"))
	h.tick(time.Hour)
	h.tick(2 * time.Minute)
	if len(h.mail.Sent) != 1 || h.mail.Sent[0].Subject != "[octo/hello#7] ✅ Merged" {
		t.Fatalf("want the merge email, got %d emails", len(h.mail.Sent))
	}

	h.gh.mu.Lock()
	h.gh.release = "v1.1"
	h.gh.mu.Unlock()
	h.tick(time.Hour)
	h.tick(2 * time.Minute)
	if len(h.mail.Sent) != 2 {
		t.Fatalf("want 2 emails, got %d", len(h.mail.Sent))
	}
	m := h.mail.Sent[1]
	if want := "[octo/hello#7] Released in v1.1"; m.Subject != want {
		t.Errorf("subject = %q, want %q", m.Subject, want)
	}
	if !strings.Contains(m.Text, "/e/done?w=") {
		t.Errorf("release email doesn't offer Done:\n%s", m.Text)
	}

	h.tick(time.Hour)
	if h.gh.calls["/repos/octo/hello/compare/v1.1...abc123"] != 1 || len(h.mail.Sent) != 2 {
		t.Error("kept checking releases after the fix shipped")
	}
}

func TestFilterMuteDoneAndReopen(t *testing.T) {
	h := newHarness(t)
	w := h.addWatch(filterStatusOnly)

	h.gh.add(comment(1, "alice", "hi"))
	h.tick(2 * time.Minute)
	h.tick(2 * time.Minute)
	if len(h.mail.Sent) != 0 {
		t.Fatal("status-only watch got a comment email")
	}

	if err := h.app.db.SetWatchStatus(h.ctx(), w.ID, "muted"); err != nil {
		t.Fatal(err)
	}
	h.gh.add(simple("closed", 2, "alice"))
	h.tick(2 * time.Minute)
	h.tick(2 * time.Minute)
	if len(h.mail.Sent) != 0 {
		t.Fatal("muted watch got an email")
	}

	// Done items stay quiet until reopened; reopening brings them back to Active.
	h.app.db.SetWatchStatus(h.ctx(), w.ID, "done")
	h.gh.add(simple("labeled", 3, "bob"))
	h.tick(2 * time.Minute)
	h.tick(2 * time.Minute)
	if len(h.mail.Sent) != 0 {
		t.Fatal("done watch got an email")
	}
	h.gh.add(simple("reopened", 4, "bob"))
	h.tick(2 * time.Minute)
	h.tick(2 * time.Minute)
	if len(h.mail.Sent) != 1 || !strings.Contains(h.mail.Sent[0].Subject, "Reopened") {
		t.Fatalf("want a Reopened email, got %d emails", len(h.mail.Sent))
	}
	got, _ := h.app.db.WatchByID(h.ctx(), w.ID)
	if got.Status != "active" {
		t.Errorf("status after reopen = %s, want active", got.Status)
	}
}

func TestQuietHoursAndDigest(t *testing.T) {
	h := newHarness(t)
	h.app.db.UpdateUserSettings(h.ctx(), h.user.ID, UserSettings{TZ: "UTC", DefaultFilter: filterEverything,
		DefaultDelivery: "instant", QuietStart: sql.NullInt64{Int64: 14 * 60, Valid: true},
		QuietEnd: sql.NullInt64{Int64: 16 * 60, Valid: true}, DigestHour: 8})
	h.addWatch(filterEverything)
	h.gh.add(comment(1, "alice", "during quiet hours"))
	h.tick(2 * time.Minute) // 15:02
	h.tick(2 * time.Minute)
	if len(h.mail.Sent) != 0 {
		t.Fatal("sent during quiet hours")
	}
	h.tick(time.Hour) // past 16:00
	if len(h.mail.Sent) != 1 {
		t.Fatalf("held email not sent after quiet hours; sent=%d", len(h.mail.Sent))
	}

	// Digest delivery: one email at the digest hour (8am).
	h2 := newHarness(t)
	h2.now = time.Date(2026, 9, 24, 6, 0, 0, 0, time.UTC)
	w := h2.addWatch(filterEverything)
	h2.app.db.SetWatchPrefs(h2.ctx(), w.ID, filterEverything, "digest")
	h2.gh.add(comment(1, "alice", "one"))
	h2.gh.add(comment(2, "bob", "two"))
	h2.tick(10 * time.Minute)
	if len(h2.mail.Sent) != 0 {
		t.Fatal("digest sent before digest hour")
	}
	h2.tick(2 * time.Hour) // 08:10
	h2.tick(time.Minute)
	h2.tick(time.Hour) // only one per day
	if len(h2.mail.Sent) != 1 || !strings.Contains(h2.mail.Sent[0].Subject, "digest") {
		t.Fatalf("want one digest, got %d", len(h2.mail.Sent))
	}
	if !strings.Contains(h2.mail.Sent[0].Text, "alice commented") || !strings.Contains(h2.mail.Sent[0].Text, "bob commented") {
		t.Errorf("digest missing events:\n%s", h2.mail.Sent[0].Text)
	}
}

func TestTimelineShrinks(t *testing.T) {
	h := newHarness(t)
	for i := 1; i <= 5; i++ {
		h.gh.add(comment(i, "a", "x"))
	}
	h.addWatch(filterEverything)
	// Deleting events means the last page we read (3) is now empty.
	h.gh.mu.Lock()
	h.gh.timeline = h.gh.timeline[:3]
	h.gh.mu.Unlock()
	h.gh.add(comment(9, "zed", "after deletes"))
	h.tick(2 * time.Minute)
	h.tick(2 * time.Minute)
	if len(h.mail.Sent) != 1 || !strings.Contains(h.mail.Sent[0].Subject, "zed") {
		t.Fatalf("missed the event after the timeline shrank; sent=%d", len(h.mail.Sent))
	}
}

// ---- HTTP ----

func (h *harness) client() *http.Client {
	return &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func (h *harness) do(method, path string, form url.Values) (*http.Response, string) {
	h.t.Helper()
	var body io.Reader
	if form != nil {
		form.Set("csrf", h.app.keys.CSRF(h.user.ID))
		body = strings.NewReader(form.Encode())
	}
	req, _ := http.NewRequest(method, h.srv.URL+path, body)
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: h.app.keys.SessionValue(h.user.ID, time.Now())})
	resp, err := h.client().Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, string(b)
}

func TestWebFlow(t *testing.T) {
	h := newHarness(t)
	h.app.now = time.Now
	h.gh.add(comment(1, "alice", "hello"))

	resp, body := h.do("GET", "/items", nil)
	if resp.StatusCode != 200 || !strings.Contains(body, "Nothing watched yet") {
		t.Fatalf("empty list: %d\n%s", resp.StatusCode, body)
	}
	resp, body = h.do("GET", "/items/new?url="+url.QueryEscape("https://github.com/octo/hello/issues/7"), nil)
	if resp.StatusCode != 200 || !strings.Contains(body, "Flaky shutdown") {
		t.Fatalf("preview: %d\n%s", resp.StatusCode, body)
	}
	resp, body = h.do("POST", "/items", url.Values{"url": {"https://github.com/octo/hello/issues/7"}, "note": {""}, "cat": {"state"}})
	if resp.StatusCode != 422 || !strings.Contains(body, "Add a note") {
		t.Fatalf("empty note accepted: %d", resp.StatusCode)
	}
	resp, _ = h.do("POST", "/items", url.Values{"url": {"https://github.com/octo/hello/issues/7"}, "note": {"Blocks v2"}, "cat": {"state", "comments"}, "delivery": {"instant"}})
	if resp.StatusCode != 303 {
		t.Fatalf("create: %d", resp.StatusCode)
	}
	loc := resp.Header.Get("Location")
	resp, body = h.do("GET", loc, nil)
	if resp.StatusCode != 200 || !strings.Contains(body, "Blocks v2") || !strings.Contains(body, "alice commented") {
		t.Fatalf("detail: %d\n%s", resp.StatusCode, body)
	}
	// Watching again redirects to the existing item.
	resp, _ = h.do("GET", "/items/new?url=octo/hello%237", nil)
	if resp.StatusCode != 303 || resp.Header.Get("Location") != loc {
		t.Fatalf("re-watch should redirect to %s, got %d %s", loc, resp.StatusCode, resp.Header.Get("Location"))
	}
	resp, _ = h.do("POST", loc+"/note", url.Values{"note": {"Blocks **v3** now <script>x</script>"}})
	if resp.StatusCode != 303 {
		t.Fatalf("edit note: %d", resp.StatusCode)
	}
	_, body = h.do("GET", loc, nil)
	if !strings.Contains(body, "Blocks <strong>v3</strong> now") || strings.Contains(body, "<script>x") {
		t.Fatalf("note not rendered as safe markdown:\n%s", body)
	}
	_, body = h.do("GET", "/items?q=v3", nil)
	if !strings.Contains(body, "Blocks <strong>v3</strong> now") {
		t.Fatal("search by note failed")
	}
	// Missing CSRF is rejected.
	req, _ := http.NewRequest("POST", h.srv.URL+loc+"/delete", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: h.app.keys.SessionValue(h.user.ID, time.Now())})
	if r, _ := h.client().Do(req); r.StatusCode != 403 {
		t.Fatalf("delete without csrf: %d", r.StatusCode)
	}
	for _, p := range []string{"/settings", "/", "/manifest.webmanifest", "/icon-192.png", "/watchnote.user.js", "/static/style.css"} {
		if resp, _ := h.do("GET", p, nil); resp.StatusCode >= 400 {
			t.Errorf("GET %s: %d", p, resp.StatusCode)
		}
	}
}

func TestDoneBadge(t *testing.T) {
	h := newHarness(t)
	w := h.addWatch(filterEverything)
	_, body := h.do("GET", fmt.Sprintf("/items/%d", w.ID), nil)
	badge := fmt.Sprintf("/badge/%d?s=%s", w.ID, h.app.keys.LinkSig("badge", w.ID))
	if !strings.Contains(body, badge) {
		t.Fatalf("detail page doesn't offer the badge %s", badge)
	}
	get := func(path string) (*http.Response, string) {
		resp, err := http.Get(h.srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp, string(b)
	}
	resp, svg := get(badge)
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "image/svg+xml" || !strings.Contains(svg, ">not done<") {
		t.Fatalf("active badge: %d %s\n%s", resp.StatusCode, resp.Header.Get("Content-Type"), svg)
	}
	if !strings.Contains(resp.Header.Get("Cache-Control"), "no-cache") {
		t.Error("badge can be cached")
	}
	h.do("POST", fmt.Sprintf("/items/%d/status", w.ID), url.Values{"status": {"done"}})
	if _, svg = get(badge); !strings.Contains(svg, ">done<") {
		t.Fatalf("done badge:\n%s", svg)
	}
	if resp, _ = get(fmt.Sprintf("/badge/%d?s=bad", w.ID)); resp.StatusCode != 403 {
		t.Errorf("bad signature: %d", resp.StatusCode)
	}
}

func TestEmailActionLinks(t *testing.T) {
	h := newHarness(t)
	w := h.addWatch(filterEverything)
	sig := h.app.keys.LinkSig("stop", w.ID)
	get := func(path string) int {
		resp, err := http.Get(h.srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := get(fmt.Sprintf("/e/stop?w=%d&s=bad", w.ID)); code != 403 {
		t.Errorf("bad signature: %d", code)
	}
	if code := get(fmt.Sprintf("/e/mute?w=%d&s=%s", w.ID, sig)); code != 403 {
		t.Errorf("signature reused for another action: %d", code)
	}
	// GET only confirms (link scanners must not unsubscribe people).
	if code := get(fmt.Sprintf("/e/stop?w=%d&s=%s", w.ID, sig)); code != 200 {
		t.Fatalf("confirm page: %d", code)
	}
	if _, err := h.app.db.WatchByID(h.ctx(), w.ID); err != nil {
		t.Fatal("GET deleted the watch")
	}
	// RFC 8058 one-click POST.
	resp, err := http.Post(h.srv.URL+fmt.Sprintf("/e/stop?w=%d&s=%s", w.ID, sig), "application/x-www-form-urlencoded",
		strings.NewReader("List-Unsubscribe=One-Click"))
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("one-click: %v %d", err, resp.StatusCode)
	}
	if _, err := h.app.db.WatchByID(h.ctx(), w.ID); err != errNotFound {
		t.Fatal("watch not deleted")
	}
	if it, _ := h.app.db.ItemByRef(h.ctx(), "octo", "hello", 7); it != nil {
		t.Error("orphan item not cleaned up")
	}
}

func TestAPI(t *testing.T) {
	h := newHarness(t)
	tok := "wn_test"
	h.app.db.SetAPITokenHash(h.ctx(), h.user.ID, hashToken(tok))
	call := func(method, path, body, token string) (int, map[string]any) {
		req, _ := http.NewRequest(method, h.srv.URL+path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}
	u := url.QueryEscape("https://github.com/octo/hello/pull/7/files")
	if code, _ := call("GET", "/api/watch?url="+u, "", "nope"); code != 401 {
		t.Errorf("bad token: %d", code)
	}
	if code, out := call("GET", "/api/watch?url="+u, "", tok); code != 200 || out["watching"] != false {
		t.Errorf("lookup before: %d %v", code, out)
	}
	code, out := call("POST", "/api/watch", `{"url":"https://github.com/octo/hello/issues/7","note":"from GitHub","preset":"status"}`, tok)
	if code != 201 || out["watching"] != true {
		t.Fatalf("create: %d %v", code, out)
	}
	if code, out := call("GET", "/api/watch?url="+u, "", tok); code != 200 || out["note"] != "from GitHub" {
		t.Errorf("lookup after: %d %v", code, out)
	}
	if code, _ := call("POST", "/api/watch", `{"url":"https://github.com/octo/missing/issues/1","note":"x"}`, tok); code != 404 {
		t.Errorf("missing item: %d", code)
	}
}

// ---- units ----

func TestParseRef(t *testing.T) {
	for in, want := range map[string]string{
		"https://github.com/rust-lang/rust/pull/12744":          "rust-lang/rust#12744",
		"github.com/cli/cli/issues/9021#issuecomment-1":         "cli/cli#9021",
		"https://github.com/a/b.js/pull/3/files":                "a/b.js#3",
		"Look at this — https://github.com/x/y/issues/5 thanks": "x/y#5",
		"vercel/next.js#58112":                                  "vercel/next.js#58112",
	} {
		r, ok := parseRef(in)
		if !ok || r.String() != want {
			t.Errorf("parseRef(%q) = %v %v, want %s", in, r, ok, want)
		}
	}
	for _, in := range []string{"", "https://github.com/a/b", "https://gitlab.com/a/b/issues/1", "a/b#0"} {
		if _, ok := parseRef(in); ok {
			t.Errorf("parseRef(%q) should fail", in)
		}
	}
}

func TestClassify(t *testing.T) {
	now := time.Now()
	cases := []struct {
		raw, cat, summary string
	}{
		{`{"event":"reviewed","id":1,"state":"approved","user":{"login":"steve"},"submitted_at":"2026-01-01T00:00:00Z"}`, "reviews", "steve approved these changes"},
		{`{"event":"committed","sha":"abc","message":"Fix it\n\nlong body","author":{"name":"Ann","date":"2026-01-01T00:00:00Z"}}`, "commits", "Commit pushed: Fix it"},
		{`{"event":"merged","id":2,"actor":{"login":"tim"},"commit_id":"0123456789"}`, "state", "tim merged this (0123456)"},
		{`{"event":"closed","id":3,"actor":{"login":"tim"},"state_reason":"not_planned"}`, "state", "tim closed this as not planned"},
		{`{"event":"cross-referenced","actor":{"login":"k"},"source":{"issue":{"id":9,"number":4,"title":"Fix","repository":{"full_name":"o/r"}}}}`, "other", "k mentioned this in o/r#4: Fix"},
		{`{"event":"labeled","id":5,"actor":{"login":"b"},"label":{"name":"bug"}}`, "labels", "b added label bug"},
	}
	for _, c := range cases {
		e, ok := classify(json.RawMessage(c.raw), now)
		if !ok || e.Category != c.cat || e.Summary != c.summary {
			t.Errorf("classify(%s) = %v %q %q", c.raw, ok, e.Category, e.Summary)
		}
		if e.GHKey == "" {
			t.Errorf("no key for %s", c.raw)
		}
	}
	if _, ok := classify(json.RawMessage(`{"event":"subscribed","id":1}`), now); ok {
		t.Error("subscribed should be ignored")
	}
}

func TestQuietHoursWrap(t *testing.T) {
	u := &User{TZ: "America/New_York", QuietStart: sql.NullInt64{Int64: 22 * 60, Valid: true}, QuietEnd: sql.NullInt64{Int64: 7 * 60, Valid: true}}
	ny, _ := time.LoadLocation("America/New_York")
	for hour, want := range map[int]bool{21: false, 22: true, 2: true, 6: true, 7: false, 12: false} {
		if got := inQuietHours(u, time.Date(2026, 9, 24, hour, 30, 0, 0, ny)); got != want {
			t.Errorf("%d:30 quiet=%v want %v", hour, got, want)
		}
	}
}

func TestSessionsAndLinks(t *testing.T) {
	k := newKeys(strings.Repeat("s", 32))
	now := time.Now()
	v := k.SessionValue(42, now)
	if uid, ok := k.ParseSession(v, now); !ok || uid != 42 {
		t.Fatal("round trip failed")
	}
	if _, ok := k.ParseSession(v, now.Add(sessionTTL+time.Minute)); ok {
		t.Error("expired session accepted")
	}
	if _, ok := k.ParseSession(strings.Replace(v, "42.", "43.", 1), now); ok {
		t.Error("tampered session accepted")
	}
	if newKeys(strings.Repeat("t", 32)).CheckLink("stop", 1, k.LinkSig("stop", 1)) {
		t.Error("link valid under another secret")
	}
	sealed, _ := k.Seal("ghp_secret")
	if plain, err := k.Open(sealed); err != nil || plain != "ghp_secret" {
		t.Error("seal round trip failed")
	}
}
