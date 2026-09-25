package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// The MCP server at /mcp lets an AI assistant read and manage your watches.
// It authenticates with the same personal token as the userscript. Each
// request builds a server bound to the token's user (the transport is
// stateless), so one user's tools can never see another's watches.

type ctxUserKey struct{}

func (a *App) mcpHandler() http.Handler {
	h := mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server {
		return a.mcpServer(r.Context().Value(ctxUserKey{}).(*User))
	}, &mcp.StreamableHTTPOptions{Stateless: true})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, err := a.apiUser(r)
		if err != nil {
			w.Header().Set("WWW-Authenticate", `Bearer realm="watchnote"`)
			apiError(w, http.StatusUnauthorized, err.Error())
			return
		}
		h.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxUserKey{}, u)))
	})
}

type mcpWatch struct {
	ID        int64  `json:"id"`
	Ref       string `json:"ref" jsonschema:"owner/repo#number"`
	Kind      string `json:"kind" jsonschema:"issue or pr"`
	Title     string `json:"title"`
	State     string `json:"state" jsonschema:"open, closed or merged"`
	Note      string `json:"note" jsonschema:"why the user is watching it, in Markdown, followed by any code comments that reference it"`
	Status    string `json:"status" jsonschema:"active, muted or done"`
	Filter    string `json:"filter" jsonschema:"comma-separated kinds of update that are emailed"`
	Delivery  string `json:"delivery" jsonschema:"instant or digest"`
	NewEvents int    `json:"new_events" jsonschema:"updates since the user last opened it in Watchnote"`
	GitHubURL string `json:"github_url"`
	AppURL    string `json:"app_url"`
	SavedAt   string `json:"saved_at"`
}

type mcpEvent struct {
	Summary  string `json:"summary"`
	Category string `json:"category"`
	Body     string `json:"body,omitempty"`
	URL      string `json:"url,omitempty"`
	At       string `json:"at"`
}

func (a *App) toMCPWatch(w *Watch) mcpWatch {
	return mcpWatch{
		ID: w.ID, Ref: w.Item.Ref(), Kind: w.Item.Kind, Title: w.Item.Title, State: w.Item.State,
		Note: w.Why(), Status: w.Status, Filter: w.Filter, Delivery: w.Delivery, NewEvents: w.NewCount,
		GitHubURL: w.Item.HTMLURL, AppURL: fmt.Sprintf("%s/items/%d", a.cfg.BaseURL, w.ID),
		SavedAt: time.Unix(w.CreatedAt, 0).UTC().Format(time.RFC3339),
	}
}

type listWatchesIn struct {
	Status string `json:"status,omitempty" jsonschema:"active (default), done or muted"`
	Query  string `json:"query,omitempty" jsonschema:"only watches whose title, note or repo contains this"`
}

type listWatchesOut struct {
	Watches []mcpWatch `json:"watches"`
}

type watchIDIn struct {
	ID int64 `json:"id" jsonschema:"the watch's id, from list_watches"`
}

type getWatchOut struct {
	Watch  mcpWatch   `json:"watch"`
	Events []mcpEvent `json:"events" jsonschema:"recent activity, newest first"`
}

type watchIn struct {
	URL    string `json:"url" jsonschema:"a GitHub issue or pull request URL, or owner/repo#number"`
	Note   string `json:"note" jsonschema:"why the user wants to watch it, in Markdown; it heads every email about it"`
	Preset string `json:"preset,omitempty" jsonschema:"everything or status (only merged/closed/reopened); the user's default if omitted"`
}

type watchOut struct {
	Watch   mcpWatch `json:"watch"`
	Created bool     `json:"created" jsonschema:"false if the user was already watching it, in which case nothing changed"`
}

type updateNoteIn struct {
	ID   int64  `json:"id"`
	Note string `json:"note" jsonschema:"the new note, in Markdown"`
}

type setStatusIn struct {
	ID     int64  `json:"id"`
	Status string `json:"status" jsonschema:"active, muted (no emails) or done (no emails unless reopened)"`
}

type stopOut struct {
	Stopped string `json:"stopped" jsonschema:"the ref that is no longer watched"`
}

func (a *App) mcpServer(u *User) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "watchnote", Title: "Watchnote", Version: version}, &mcp.ServerOptions{
		Instructions: "Watchnote emails the user when GitHub issues and PRs they watch change. Each watch has a " +
			"Markdown note saying why the user cares; read it before acting on a watch, and write one that will make sense " +
			"to them months later when adding a watch. If the reason is code in a git repo, don't call watch: add a comment " +
			"on its own line above that code, like `// owner/repo#123: why this code depends on it`. Watchnote scans " +
			"the repos the user's saved GitHub token can push to, and deleting the comment stops the watch.",
	})
	own := func(ctx context.Context, id int64) (*Watch, error) {
		w, err := a.db.UserWatch(ctx, u.ID, id)
		if errors.Is(err, errNotFound) {
			return nil, fmt.Errorf("no watch with id %d", id)
		}
		return w, err
	}
	readOnly := &mcp.ToolAnnotations{ReadOnlyHint: true}

	mcp.AddTool(s, &mcp.Tool{Name: "list_watches", Description: "List the issues and PRs the user watches, with their notes.",
		Annotations: readOnly},
		func(ctx context.Context, _ *mcp.CallToolRequest, in listWatchesIn) (*mcp.CallToolResult, listWatchesOut, error) {
			status := in.Status
			if status == "" {
				status = "active"
			}
			if status != validTab(status) {
				return nil, listWatchesOut{}, fmt.Errorf("status must be active, done or muted")
			}
			ws, err := a.db.ListWatches(ctx, u.ID, status, in.Query)
			out := listWatchesOut{Watches: []mcpWatch{}}
			for _, w := range ws {
				out.Watches = append(out.Watches, a.toMCPWatch(w))
			}
			return nil, out, err
		})

	mcp.AddTool(s, &mcp.Tool{Name: "get_watch", Description: "Get one watch with its note and recent activity.",
		Annotations: readOnly},
		func(ctx context.Context, _ *mcp.CallToolRequest, in watchIDIn) (*mcp.CallToolResult, getWatchOut, error) {
			w, err := own(ctx, in.ID)
			if err != nil {
				return nil, getWatchOut{}, err
			}
			evs, err := a.db.RecentEvents(ctx, w.ItemID, 30)
			out := getWatchOut{Watch: a.toMCPWatch(w), Events: []mcpEvent{}}
			for _, e := range evs {
				out.Events = append(out.Events, mcpEvent{Summary: e.Summary, Category: e.Category, Body: e.Body,
					URL: e.URL, At: time.Unix(e.OccurredAt, 0).UTC().Format(time.RFC3339)})
			}
			return nil, out, err
		})

	mcp.AddTool(s, &mcp.Tool{Name: "watch", Description: "Start watching a GitHub issue or PR, with a note on why. " +
		"If the reason is code in a git repo, add an `// owner/repo#123: why` comment above that code instead.",
		Annotations: &mcp.ToolAnnotations{IdempotentHint: true, OpenWorldHint: ptr(true)}},
		func(ctx context.Context, _ *mcp.CallToolRequest, in watchIn) (*mcp.CallToolResult, watchOut, error) {
			ref, ok := parseRef(in.URL)
			note := cleanNote(in.Note)
			switch {
			case !ok:
				return nil, watchOut{}, errors.New("not a GitHub issue or PR URL")
			case note == "":
				return nil, watchOut{}, errors.New("a note is required: say why the user wants to watch this")
			}
			filter, err := presetFilter(in.Preset, u.DefaultFilter)
			if err != nil {
				return nil, watchOut{}, err
			}
			w, created, err := a.AddWatch(ctx, u, ref, note, filter, u.DefaultDelivery)
			if err != nil {
				return nil, watchOut{}, errors.New(previewError(err, u))
			}
			return nil, watchOut{Watch: a.toMCPWatch(w), Created: created}, nil
		})

	mcp.AddTool(s, &mcp.Tool{Name: "update_note", Description: "Replace a watch's note. Future emails use the new one. Code comments that reference the item stay; edit those in the code.",
		Annotations: &mcp.ToolAnnotations{IdempotentHint: true, DestructiveHint: ptr(false)}},
		func(ctx context.Context, _ *mcp.CallToolRequest, in updateNoteIn) (*mcp.CallToolResult, mcpWatch, error) {
			w, err := own(ctx, in.ID)
			if err != nil {
				return nil, mcpWatch{}, err
			}
			note := cleanNote(in.Note)
			if note == "" {
				return nil, mcpWatch{}, errors.New("the note can't be empty")
			}
			if err := a.db.SetWatchNote(ctx, w.ID, note); err != nil {
				return nil, mcpWatch{}, err
			}
			w.Note = note
			return nil, a.toMCPWatch(w), nil
		})

	mcp.AddTool(s, &mcp.Tool{Name: "set_status", Description: "Mark a watch active, muted or done.",
		Annotations: &mcp.ToolAnnotations{IdempotentHint: true, DestructiveHint: ptr(false)}},
		func(ctx context.Context, _ *mcp.CallToolRequest, in setStatusIn) (*mcp.CallToolResult, mcpWatch, error) {
			w, err := own(ctx, in.ID)
			if err != nil {
				return nil, mcpWatch{}, err
			}
			if in.Status != validTab(in.Status) {
				return nil, mcpWatch{}, errors.New("status must be active, muted or done")
			}
			if err := a.db.SetWatchStatus(ctx, w.ID, in.Status); err != nil {
				return nil, mcpWatch{}, err
			}
			w.Status = in.Status
			return nil, a.toMCPWatch(w), nil
		})

	mcp.AddTool(s, &mcp.Tool{Name: "stop_watching", Description: "Stop watching an item and delete its note. Can't be undone.",
		Annotations: &mcp.ToolAnnotations{DestructiveHint: ptr(true), IdempotentHint: true}},
		func(ctx context.Context, _ *mcp.CallToolRequest, in watchIDIn) (*mcp.CallToolResult, stopOut, error) {
			w, err := own(ctx, in.ID)
			if err != nil {
				return nil, stopOut{}, err
			}
			if len(w.Refs) > 0 {
				c := w.Refs[0]
				return nil, stopOut{}, fmt.Errorf("code comments keep this watched, e.g. %s/%s:%d; delete them instead", c.Repo, c.Path, c.Line)
			}
			return nil, stopOut{Stopped: w.Item.Ref()}, a.db.DeleteWatch(ctx, w.ID)
		})

	return s
}

func presetFilter(preset, def string) (string, error) {
	switch preset {
	case "":
		return def, nil
	case "everything":
		return filterEverything, nil
	case "status":
		return filterStatusOnly, nil
	}
	return "", errors.New("preset must be everything or status")
}

func ptr[T any](v T) *T { return &v }
