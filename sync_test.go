package main

import (
	"reflect"
	"testing"
)

func TestCodeRefScan(t *testing.T) {
	h := newHarness(t)
	sealed, _ := h.app.keys.Seal("github_pat_x")
	h.app.db.SetGitHubToken(h.ctx(), h.user.ID, sealed, "me")
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
	h.app.db.SetGitHubToken(h.ctx(), h.user.ID, nil, "")
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
		"// o/r#8:",
		"// o/r#0: zero",
	}
	var got []string
	for _, l := range lines {
		if target, note, ok := parseCodeRef(l); ok {
			got = append(got, target.String()+" "+note)
		}
	}
	want := []string{"o/r#1 slashes", "o/r#2 dashes", "o/r#3 block", "o/r#4 continued block", "o/r#5 lisp"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %q", got)
	}
}

// A fine-grained token lists repos its user can push to that it can't read.
func TestCodeRefScanSkipsUnreadableRepo(t *testing.T) {
	h := newHarness(t)
	sealed, _ := h.app.keys.Seal("github_pat_x")
	h.app.db.SetGitHubToken(h.ctx(), h.user.ID, sealed, "me")
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
