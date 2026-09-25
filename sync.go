package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// Code comments can hold watches: a comment on a line of its own like
//
//	// octo/hello#7: We need this until the upstream fix ships.
//
// keeps octo/hello#7 watched for as long as the comment exists. CI posts
// `git grep -n` output for the whole repo to PUT /api/sync, so deleting the
// comment stops the watch on the next push.

type CodeRef struct {
	WatchID int64
	Target  Ref // the referenced item; only set when parsing
	Repo    string
	SHA     string
	Path    string
	Line    int
	Note    string
}

func (c CodeRef) URL() string {
	return fmt.Sprintf("https://github.com/%s/blob/%s/%s#L%d", c.Repo, c.SHA, (&url.URL{Path: c.Path}).EscapedPath(), c.Line)
}

var (
	codeRefRe = regexp.MustCompile(`^\s*(?://+|#+|--+|;+|/\*+|\*+)\s*([\w.-]+)/([\w.-]+)#(\d+):\s*(\S.*?)\s*(?:\*/)?\s*$`)
	repoRe    = regexp.MustCompile(`^[\w.-]+/[\w.-]+$`)
	shaRe     = regexp.MustCompile(`^[0-9a-f]{7,64}$`)
)

const maxCodeRefs = 1000

// parseCodeRefs reads `git grep -n` output (path:line:text) and returns the
// lines that are reference comments.
func parseCodeRefs(repo, sha, grep string) []CodeRef {
	var out []CodeRef
	for _, l := range strings.Split(grep, "\n") {
		parts := strings.SplitN(l, ":", 3)
		if len(parts) != 3 {
			continue
		}
		line, err := strconv.Atoi(parts[1])
		m := codeRefRe.FindStringSubmatch(parts[2])
		if err != nil || m == nil {
			continue
		}
		n, _ := strconv.Atoi(m[3])
		if n == 0 {
			continue
		}
		out = append(out, CodeRef{Target: Ref{m[1], m[2], n}, Repo: repo, SHA: sha, Path: parts[0], Line: line,
			Note: cleanNote(m[4])})
	}
	return out
}

type syncOut struct {
	Watching []string `json:"watching"`
	Errors   []string `json:"errors,omitempty"`
}

// apiSync replaces the code refs from one repo with the ones in the body.
// It responds 422 if any reference couldn't be watched, so CI fails on typos.
func (a *App) apiSync(w http.ResponseWriter, r *http.Request, u *User) {
	repo, sha := strings.ToLower(r.URL.Query().Get("repo")), strings.ToLower(r.URL.Query().Get("sha"))
	if !repoRe.MatchString(repo) || !shaRe.MatchString(sha) {
		apiError(w, 400, "repo=owner/name and sha=<commit> are required")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 8<<20))
	if err != nil {
		apiError(w, 400, "body too large")
		return
	}
	refs := parseCodeRefs(repo, sha, string(body))
	if len(refs) > maxCodeRefs {
		apiError(w, 400, fmt.Sprintf("more than %d references", maxCodeRefs))
		return
	}
	out, err := a.SyncCodeRefs(r.Context(), u, repo, refs)
	if err != nil {
		a.serverError(w, err)
		return
	}
	code := 200
	if len(out.Errors) > 0 {
		code = 422
	}
	writeJSON(w, code, out)
}

// SyncCodeRefs watches everything refs point at and makes refs the only code
// refs from repo. A ref that can't be watched keeps that watch's current refs
// from repo, so a GitHub outage doesn't drop watches.
func (a *App) SyncCodeRefs(ctx context.Context, u *User, repo string, refs []CodeRef) (syncOut, error) {
	out := syncOut{Watching: []string{}}
	watchIDs := map[string]int64{} // lowercase ref -> watch id, 0 if it failed
	var keep []int64
	var resolved []CodeRef
	for _, c := range refs {
		key := strings.ToLower(c.Target.String())
		id, seen := watchIDs[key]
		if !seen {
			wt, _, err := a.AddWatch(ctx, u, c.Target, "", u.DefaultFilter, u.DefaultDelivery)
			if err != nil {
				out.Errors = append(out.Errors, fmt.Sprintf("%s:%d: %s: %s", c.Path, c.Line, c.Target, previewError(err, u)))
				if it, err := a.db.ItemByRef(ctx, c.Target.Owner, c.Target.Repo, c.Target.Number); err == nil {
					if wt, err := a.db.UserWatchForItem(ctx, u.ID, it.ID); err == nil {
						keep = append(keep, wt.ID)
					}
				}
			} else {
				id = wt.ID
				out.Watching = append(out.Watching, wt.Item.Ref())
			}
			watchIDs[key] = id
		}
		if id != 0 {
			c.WatchID = id
			resolved = append(resolved, c)
		}
	}
	return out, a.db.ReplaceCodeRefs(ctx, u.ID, repo, resolved, keep)
}
