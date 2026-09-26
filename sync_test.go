package main

import (
	"context"
	"database/sql"
	"net/url"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

func (h *harness) addToken(tok string) *GitHubToken {
	h.t.Helper()
	sealed, _ := h.app.keys.Seal(tok)
	t := &GitHubToken{UserID: h.user.ID, Sealed: sealed, Login: "me"}
	if err := h.app.db.InsertGitHubToken(h.ctx(), t); err != nil {
		h.t.Fatal(err)
	}
	return t
}

func repo(name string, push bool, extra ...string) map[string]any {
	r := map[string]any{"full_name": name, "default_branch": "main", "pushed_at": "t1", "permissions": map[string]bool{"push": push}}
	for _, k := range extra {
		r[k] = true
	}
	return r
}

func TestCodeRefScan(t *testing.T) {
	h := newHarness(t)
	h.addToken("github_pat_x")
	h.gh.repos = []map[string]any{
		{"full_name": "Me/App", "default_branch": "main", "pushed_at": "t1", "permissions": map[string]bool{"push": true}},
		{"full_name": "other/readonly", "default_branch": "main", "pushed_at": "t1", "permissions": map[string]bool{"push": false}},
		{"full_name": "me/fork", "default_branch": "main", "pushed_at": "t1", "fork": true, "permissions": map[string]bool{"push": true}},
	}
	h.gh.sha = "abc1234def"
	h.gh.code = map[string]string{
		"main.go": "package main\n\n\t// octo/hello#7: Remove the retry loop once shutdown is fixed.\n" +
			"\ts := \"// not at line start octo/hello#7: nope\"\n// octo/missing#1: typo\n",
		"lib/x.py": "# Octo/Hello#7: same workaround\n",
		"logo.png": "\x00# octo/hello#7: binary\n",
	}
	scan := func() {
		t.Helper()
		if err := h.app.scanCode(h.ctx()); err != nil {
			t.Fatal(err)
		}
	}
	watches := func() []*Watch {
		t.Helper()
		ws, err := h.app.db.ListWatches(h.ctx(), h.user.ID, "active", "")
		if err != nil {
			t.Fatal(err)
		}
		return ws
	}

	scan()
	ws := watches()
	if len(ws) != 1 || ws[0].Item.Ref() != "octo/hello#7" {
		t.Fatalf("watches: %+v", ws)
	}
	want := "same workaround ([me/app/lib/x.py:1](https://github.com/me/app/blob/abc1234def/lib/x.py#L1))\n\n" +
		"Remove the retry loop once shutdown is fixed. ([me/app/main.go:3](https://github.com/me/app/blob/abc1234def/main.go#L3))"
	if got := ws[0].Why(); got != want {
		t.Errorf("why:\n%s", got)
	}
	if found, _ := h.app.db.ListWatches(h.ctx(), h.user.ID, "active", "retry loop"); len(found) != 1 {
		t.Error("search doesn't match code notes")
	}
	if n := h.gh.calls["/repos/other/readonly/git/ref/heads/main"] + h.gh.calls["/repos/me/fork/git/ref/heads/main"]; n != 0 {
		t.Error("scanned a read-only repo or a fork")
	}

	// No push since, so no rescan.
	scan()
	if n := h.gh.calls["/repos/me/app/tarball/abc1234def"]; n != 1 {
		t.Errorf("tarball fetched %d times", n)
	}

	// Deleting the comments stops the watch.
	h.gh.repos[0]["pushed_at"], h.gh.sha = "t2", "fff0000aaa"
	h.gh.code = map[string]string{"main.go": "package main\n"}
	scan()
	if len(watches()) != 0 {
		t.Fatal("watch outlived its comments")
	}
	if _, err := h.app.db.ItemByRef(h.ctx(), "octo", "hello", 7); err != errNotFound {
		t.Error("orphan item kept")
	}

	// A watch with its own note outlives its comments, including when the token goes.
	if _, _, err := h.app.AddWatch(h.ctx(), h.user, Ref{"octo", "hello", 7}, "mine", filterEverything, "instant"); err != nil {
		t.Fatal(err)
	}
	h.gh.repos[0]["pushed_at"], h.gh.sha = "t3", "eee1111bbb"
	h.gh.code = map[string]string{"a.go": "// octo/hello#7: from code\n"}
	scan()
	if ws := watches(); len(ws) != 1 || ws[0].Why() != "mine\n\nfrom code ([me/app/a.go:1](https://github.com/me/app/blob/eee1111bbb/a.go#L1))" {
		t.Fatalf("attached: %+v", ws)
	}
	h.app.db.Exec(`DELETE FROM github_tokens`)
	scan()
	if ws := watches(); len(ws) != 1 || ws[0].Note != "mine" || len(ws[0].Refs) != 0 {
		t.Fatalf("detached: %+v", ws)
	}
	if repos, _ := h.app.db.CodeRepos(h.ctx(), h.user.ID); len(repos) != 0 {
		t.Errorf("repos kept: %v", repos)
	}
}

func TestParseCodeRef(t *testing.T) {
	lines := []string{
		"// o/r#1: slashes",
		"  -- o/r#2: dashes",
		"/* o/r#3: block */",
		" * o/r#4: continued block",
		";; o/r#5: lisp",
		"x() // o/r#6: trailing",
		"+  // o/r#7: added by a patch",
		"-  // o/r#9: removed by a patch",
		"// o/r#8:",
		"// o/r#0: zero",
		"//! o/r#10: rust inner doc",
		"o/r#11: inside a docstring",
		"<!-- o/r#12: html -->",
		"% o/r#13: tex",
		"see (o/r#14: mid-sentence",
		"o/r#15:3 no note",
	}
	var got []string
	for _, l := range lines {
		if target, note, ok := parseCodeRef(l); ok {
			got = append(got, target.String()+" "+note)
		}
	}
	want := []string{"o/r#1 slashes", "o/r#2 dashes", "o/r#3 block", "o/r#4 continued block", "o/r#5 lisp", "o/r#7 added by a patch",
		"o/r#10 rust inner doc", "o/r#11 inside a docstring", "o/r#12 html", "o/r#13 tex"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %q", got)
	}
}

// A fine-grained token lists repos its user can push to that it can't read.
func TestCodeRefScanSkipsUnreadableRepo(t *testing.T) {
	h := newHarness(t)
	h.addToken("github_pat_x")
	h.gh.repos = []map[string]any{
		{"full_name": "me/private", "default_branch": "main", "pushed_at": "t1", "permissions": map[string]bool{"push": true}},
	}
	for range 2 {
		if err := h.app.scanCode(h.ctx()); err != nil {
			t.Fatal(err)
		}
	}
	if n := h.gh.calls["/repos/me/private/git/ref/heads/main"]; n != 1 {
		t.Errorf("unreadable repo scanned %d times before its next push", n)
	}
}

// Fine-grained tokens each cover one owner, so a user can save several.
func TestCodeRefScanManyTokens(t *testing.T) {
	h := newHarness(t)
	h.app.now = time.Now
	h.gh.sha = "abc1234def"
	h.gh.code = map[string]string{"main.go": "// octo/hello#7: from code\n"}
	h.gh.reposFor = map[string][]map[string]any{
		"pat_me":   {repo("me/app", true)},
		"pat_acme": {repo("me/app", true), repo("acme/docs", false), repo("Me/Private", true, "private")},
	}
	for _, tok := range []string{"pat_me", "pat_acme"} {
		if resp, body := h.do("POST", "/settings/github", url.Values{"token": {tok}}); resp.StatusCode != 303 {
			t.Fatalf("add %s: %d\n%s", tok, resp.StatusCode, body)
		}
	}
	if resp, _ := h.do("POST", "/settings/github", url.Values{"token": {"pat_me"}}); !strings.Contains(resp.Header.Get("Location"), "already+saved") {
		t.Errorf("saved the same token twice: %s", resp.Header.Get("Location"))
	}
	scan := func() {
		t.Helper()
		if err := h.app.scanCode(h.ctx()); err != nil {
			t.Fatal(err)
		}
	}
	watched := func() bool {
		ws, _ := h.app.db.ListWatches(h.ctx(), h.user.ID, "active", "")
		return len(ws) == 1 && len(ws[0].Refs) == 1
	}

	scan()
	if !watched() {
		t.Fatal("comment in me/app not watched")
	}
	if n := h.gh.calls["/repos/me/app/tarball/abc1234def"]; n != 1 {
		t.Errorf("repo both tokens see fetched %d times", n)
	}
	if n := h.gh.calls["/repos/me/private/git/ref/heads/main"]; n != 1 {
		t.Errorf("second token's repo checked %d times", n)
	}
	tokens, _ := h.app.db.GitHubTokens(h.ctx(), h.user.ID)
	if len(tokens) != 2 || len(tokens[1].Scanned()) != 2 || !reflect.DeepEqual(tokens[1].Owners(), []string{"me", "acme"}) {
		t.Fatalf("tokens: %+v", tokens)
	}

	// Settings lists what each token scans.
	_, body := h.do("GET", "/settings", nil)
	for _, s := range []string{"Scans <b>1 repo</b>", "Scans <b>2 repos</b>", "Me/Private", "can't read its code",
		"It can also read 1 repo", "Token for @octocat"} {
		if !strings.Contains(body, s) {
			t.Errorf("settings missing %q", s)
		}
	}
	if strings.Contains(body, ">acme/docs<") {
		t.Error("settings lists a repo that isn't scanned")
	}

	// While a token can't list its repos, what it listed last keeps its refs.
	h.gh.reposFor["pat_me"] = nil
	delete(h.gh.reposFor, "pat_acme")
	h.gh.down = map[string]bool{"pat_acme": true}
	scan()
	if !watched() {
		t.Fatal("refs dropped while a token couldn't list its repos")
	}
	if _, body := h.do("GET", "/settings", nil); !strings.Contains(body, "GitHub didn't list this token's repos") {
		t.Error("listing failure not shown")
	}
	h.gh.reposFor["pat_acme"] = []map[string]any{repo("acme/docs", false)}
	scan()
	if watched() {
		t.Fatal("refs kept from a repo no token lists")
	}

	// Removing a token takes only that token.
	if resp, _ := h.do("POST", "/settings/github/"+strconv.FormatInt(tokens[0].ID, 10)+"/remove", url.Values{}); resp.StatusCode != 303 {
		t.Fatalf("remove: %d", resp.StatusCode)
	}
	if left, _ := h.app.db.GitHubTokens(h.ctx(), h.user.ID); len(left) != 1 || left[0].ID != tokens[1].ID {
		t.Fatalf("left: %+v", left)
	}
}

// Before a user could save several tokens, users held one.
func TestMoveTokens(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	old, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`CREATE TABLE users (id INTEGER PRIMARY KEY, google_sub TEXT NOT NULL UNIQUE, email TEXT NOT NULL,
			name TEXT NOT NULL DEFAULT '', picture TEXT NOT NULL DEFAULT '', tz TEXT NOT NULL DEFAULT '',
			default_filter TEXT NOT NULL DEFAULT '', default_delivery TEXT NOT NULL DEFAULT 'instant',
			quiet_start INTEGER, quiet_end INTEGER, digest_hour INTEGER NOT NULL DEFAULT 8,
			last_digest_day TEXT NOT NULL DEFAULT '', github_token BLOB, github_login TEXT NOT NULL DEFAULT '',
			api_token_hash TEXT UNIQUE, created_at INTEGER NOT NULL)`,
		`INSERT INTO users (google_sub, email, github_token, github_login, created_at) VALUES ('a', 'a@x', x'0102', 'octocat', 1)`,
		`INSERT INTO users (google_sub, email, created_at) VALUES ('b', 'b@x', 1)`,
	} {
		if _, err := old.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	old.Close()
	for range 2 {
		db, err := openDB(path)
		if err != nil {
			t.Fatal(err)
		}
		tokens, _ := db.GitHubTokens(context.Background(), 1)
		users, _ := db.AllUsers(context.Background())
		db.Close()
		if len(tokens) != 1 || tokens[0].Login != "octocat" || string(tokens[0].Sealed) != string([]byte{1, 2}) {
			t.Fatalf("tokens: %+v", tokens)
		}
		if len(users) != 2 || !users[0].HasGitHubToken || users[1].HasGitHubToken {
			t.Fatalf("users: %+v %+v", users[0], users[1])
		}
	}
}
