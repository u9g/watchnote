package main

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

func TestCodeRefSync(t *testing.T) {
	h := newHarness(t)
	tok := "wn_test"
	h.app.db.SetAPITokenHash(h.ctx(), h.user.ID, hashToken(tok))
	sync := func(body string) (int, syncOut) {
		req, _ := http.NewRequest("PUT", h.srv.URL+"/api/sync?repo=Me/App&sha=abc1234", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+tok)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out syncOut
		json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}
	watches := func() []*Watch {
		ws, err := h.app.db.ListWatches(h.ctx(), h.user.ID, "active", "")
		if err != nil {
			t.Fatal(err)
		}
		return ws
	}

	code, out := sync("main.go:12:\t// octo/hello#7: Remove the retry loop once shutdown is fixed.\n" +
		"lib/x.py:3:    # Octo/Hello#7: same workaround\n" +
		`main.go:40:	s := "// not at line start octo/hello#7: nope"` + "\n")
	if code != 200 || !reflect.DeepEqual(out.Watching, []string{"octo/hello#7"}) {
		t.Fatalf("sync: %d %+v", code, out)
	}
	ws := watches()
	if len(ws) != 1 || len(ws[0].Refs) != 2 {
		t.Fatalf("watches: %+v", ws)
	}
	want := "same workaround ([me/app/lib/x.py:3](https://github.com/me/app/blob/abc1234/lib/x.py#L3))\n\n" +
		"Remove the retry loop once shutdown is fixed. ([me/app/main.go:12](https://github.com/me/app/blob/abc1234/main.go#L12))"
	if got := ws[0].Why(); got != want {
		t.Errorf("why:\n%s", got)
	}
	if found, _ := h.app.db.ListWatches(h.ctx(), h.user.ID, "active", "retry loop"); len(found) != 1 {
		t.Error("search doesn't match code notes")
	}

	// A bad reference fails the sync but the rest still applies.
	code, out = sync("main.go:12:// octo/hello#7: moved\nmain.go:20:// octo/missing#1: typo\n")
	if code != 422 || len(out.Errors) != 1 || !strings.HasPrefix(out.Errors[0], "main.go:20: octo/missing#1:") {
		t.Fatalf("bad ref: %d %+v", code, out)
	}
	if ws := watches(); len(ws) != 1 || len(ws[0].Refs) != 1 || ws[0].Refs[0].Note != "moved" {
		t.Fatalf("after bad ref: %+v", ws)
	}

	// Deleting the comment stops the watch.
	if code, _ := sync(""); code != 200 || len(watches()) != 0 {
		t.Fatalf("empty sync: %d %d", code, len(watches()))
	}
	if _, err := h.app.db.ItemByRef(h.ctx(), "octo", "hello", 7); err != errNotFound {
		t.Error("orphan item kept")
	}

	// A watch with its own note outlives its comments.
	if _, _, err := h.app.AddWatch(h.ctx(), h.user, Ref{"octo", "hello", 7}, "mine", filterEverything, "instant"); err != nil {
		t.Fatal(err)
	}
	sync("a.go:1:// octo/hello#7: from code\n")
	if ws := watches(); len(ws) != 1 || ws[0].Why() != "mine\n\nfrom code ([me/app/a.go:1](https://github.com/me/app/blob/abc1234/a.go#L1))" {
		t.Fatalf("attached: %+v", ws)
	}
	sync("")
	if ws := watches(); len(ws) != 1 || ws[0].Note != "mine" || len(ws[0].Refs) != 0 {
		t.Fatalf("detached: %+v", ws)
	}

	if code, _ := sync("x"); code != 200 {
		t.Errorf("no refs: %d", code)
	}
	req, _ := http.NewRequest("PUT", h.srv.URL+"/api/sync?repo=me", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	if resp, _ := http.DefaultClient.Do(req); resp.StatusCode != 400 {
		t.Errorf("missing sha: %d", resp.StatusCode)
	}
}

func TestParseCodeRefs(t *testing.T) {
	in := strings.Join([]string{
		"a.go:1:// o/r#1: slashes",
		"a.sql:2:  -- o/r#2: dashes",
		"a.c:3:/* o/r#3: block */",
		"a.c:4: * o/r#4: continued block",
		"a.el:5:;; o/r#5: lisp",
		"a.go:6:x() // o/r#6: trailing",
		"a.go:7:// o/r#8:",
		"a.go:8:// o/r#0: zero",
		"a:b.go:9:// o/r#9: colon in path",
	}, "\n")
	var got []string
	for _, c := range parseCodeRefs("me/app", "abc1234", in) {
		got = append(got, c.Target.String()+" "+c.Path+" "+c.Note)
	}
	want := []string{"o/r#1 a.go slashes", "o/r#2 a.sql dashes", "o/r#3 a.c block", "o/r#4 a.c continued block",
		"o/r#5 a.el lisp"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %q", got)
	}
}
