package main

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type bearer struct{ token string }

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return http.DefaultTransport.RoundTrip(r)
}

func (h *harness) mcpSession(token string) (*mcp.ClientSession, error) {
	c := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	return c.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint: h.srv.URL + "/mcp", HTTPClient: &http.Client{Transport: bearer{token}},
	}, nil)
}

// call runs a tool and decodes its structured output. isErr reports a tool error.
func call(t *testing.T, s *mcp.ClientSession, name string, args map[string]any, out any) (isErr bool, text string) {
	t.Helper()
	res, err := s.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			text += tc.Text
		}
	}
	if !res.IsError && out != nil {
		b, _ := json.Marshal(res.StructuredContent)
		if err := json.Unmarshal(b, out); err != nil {
			t.Fatalf("%s: decode: %v", name, err)
		}
	}
	return res.IsError, text
}

func TestMCP(t *testing.T) {
	h := newHarness(t)
	h.app.db.SetAPITokenHash(h.ctx(), h.user.ID, hashToken("wn_me"))
	h.gh.add(comment(1, "alice", "hello"))

	if _, err := h.mcpSession("wrong"); err == nil {
		t.Fatal("connected with a bad token")
	}
	s, err := h.mcpSession("wn_me")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	tools, err := s.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tl := range tools.Tools {
		names = append(names, tl.Name)
	}
	sort.Strings(names)
	if got := strings.Join(names, ","); got != "get_watch,list_watches,set_status,stop_watching,update_note,watch" {
		t.Fatalf("tools = %s", got)
	}

	if isErr, text := call(t, s, "watch", map[string]any{"url": "octo/hello#7", "note": ""}, nil); !isErr || !strings.Contains(text, "note is required") {
		t.Errorf("empty note: %v %q", isErr, text)
	}
	var w watchOut
	if isErr, text := call(t, s, "watch", map[string]any{"url": "https://github.com/octo/hello/issues/7", "note": "Blocks v2", "preset": "status"}, &w); isErr {
		t.Fatal(text)
	}
	if !w.Created || w.Watch.Filter != filterStatusOnly || w.Watch.Ref != "octo/hello#7" || w.Watch.Title != "Flaky shutdown" {
		t.Errorf("watch = %+v", w)
	}

	var list listWatchesOut
	call(t, s, "list_watches", map[string]any{"query": "v2"}, &list)
	if len(list.Watches) != 1 || list.Watches[0].Note != "Blocks v2" {
		t.Errorf("list = %+v", list)
	}
	var got getWatchOut
	call(t, s, "get_watch", map[string]any{"id": w.Watch.ID}, &got)
	if len(got.Events) != 1 || got.Events[0].Summary != "alice commented" {
		t.Errorf("events = %+v", got.Events)
	}

	var upd mcpWatch
	call(t, s, "update_note", map[string]any{"id": w.Watch.ID, "note": "Blocks v3"}, &upd)
	call(t, s, "set_status", map[string]any{"id": w.Watch.ID, "status": "done"}, &upd)
	if upd.Note != "Blocks v3" || upd.Status != "done" {
		t.Errorf("after update = %+v", upd)
	}
	if isErr, _ := call(t, s, "set_status", map[string]any{"id": w.Watch.ID, "status": "gone"}, nil); !isErr {
		t.Error("bad status accepted")
	}

	// Another user's token can't see or touch this watch.
	other, _ := h.app.db.UpsertGoogleUser(h.ctx(), "sub-2", "other@example.com", "Other", "")
	h.app.db.SetAPITokenHash(h.ctx(), other.ID, hashToken("wn_other"))
	s2, err := h.mcpSession("wn_other")
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if isErr, text := call(t, s2, "get_watch", map[string]any{"id": w.Watch.ID}, nil); !isErr || !strings.Contains(text, "no watch") {
		t.Errorf("other user read the watch: %v %q", isErr, text)
	}
	if isErr, _ := call(t, s2, "stop_watching", map[string]any{"id": w.Watch.ID}, nil); !isErr {
		t.Error("other user stopped the watch")
	}

	var stop stopOut
	call(t, s, "stop_watching", map[string]any{"id": w.Watch.ID}, &stop)
	if stop.Stopped != "octo/hello#7" {
		t.Errorf("stop = %+v", stop)
	}
	call(t, s, "list_watches", map[string]any{"status": "done"}, &list)
	if len(list.Watches) != 0 {
		t.Errorf("still listed after stop: %+v", list)
	}
}
