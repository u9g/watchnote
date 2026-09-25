package main

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// Code comments can hold watches: a comment on a line of its own like
//
//	// octo/hello#7: We need this until the upstream fix ships.
//
// keeps octo/hello#7 watched for as long as the comment is on the default
// branch. Watchnote scans the repos a user's saved GitHub token can push to,
// again whenever they're pushed to.

type CodeRef struct {
	WatchID int64
	Target  Ref // the referenced item; only set when scanning
	Repo    string
	SHA     string
	Path    string
	Line    int
	Note    string
}

func (c CodeRef) URL() string {
	return fmt.Sprintf("https://github.com/%s/blob/%s/%s#L%d", c.Repo, c.SHA, (&url.URL{Path: c.Path}).EscapedPath(), c.Line)
}

var codeRefRe = regexp.MustCompile(`^\s*(?://+|#+|--+|;+|/\*+|\*+)\s*([\w.-]+)/([\w.-]+)#(\d+):\s*(\S.*?)\s*(?:\*/)?\s*$`)

const (
	maxScanFile      = 1 << 20
	maxScansPerCycle = 10 // per user; the rest wait for the next cycle
)

// parseCodeRef reports whether line is a reference comment.
func parseCodeRef(line string) (target Ref, note string, ok bool) {
	m := codeRefRe.FindStringSubmatch(line)
	if m == nil {
		return Ref{}, "", false
	}
	n, _ := strconv.Atoi(m[3])
	return Ref{m[1], m[2], n}, cleanNote(m[4]), n > 0
}

// scanTarball finds reference comments in a GitHub tarball of repo at sha.
func scanTarball(r io.Reader, repo, sha string) ([]CodeRef, error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return nil, err
	}
	var out []CodeRef
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		// Entries sit under a single owner-repo-sha/ directory.
		_, path, _ := strings.Cut(h.Name, "/")
		if h.Typeflag != tar.TypeReg || h.Size > maxScanFile || path == "" {
			continue
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			return nil, err
		}
		if bytes.IndexByte(body[:min(len(body), 8000)], 0) >= 0 {
			continue // binary
		}
		sc := bufio.NewScanner(bytes.NewReader(body))
		sc.Buffer(nil, maxScanFile)
		for n := 1; sc.Scan(); n++ {
			if !bytes.Contains(sc.Bytes(), []byte("#")) {
				continue
			}
			if target, note, ok := parseCodeRef(sc.Text()); ok {
				out = append(out, CodeRef{Target: target, Repo: repo, SHA: sha, Path: path, Line: n, Note: note})
			}
		}
	}
}

// scanCode rescans every repo pushed to since its last scan, for each user
// with a GitHub token, and drops refs from repos the token no longer reaches.
func (a *App) scanCode(ctx context.Context) error {
	users, err := a.db.AllUsers(ctx)
	if err != nil {
		return err
	}
	for _, u := range users {
		if ctx.Err() != nil {
			return nil
		}
		if err := a.scanUserCode(ctx, u); err != nil {
			a.log.Error("code scan failed", "user", u.ID, "err", err)
		}
	}
	return nil
}

func (a *App) scanUserCode(ctx context.Context, u *User) error {
	var repos []ghRepo
	var token string
	if u.GitHubToken != nil {
		var err error
		if token, err = a.keys.Open(u.GitHubToken); err != nil {
			return err
		}
		if repos, err = a.gh.PushableRepos(ctx, token); err != nil {
			return err
		}
	}
	scanned, err := a.db.CodeRepos(ctx, u.ID)
	if err != nil {
		return err
	}
	listed := map[string]bool{}
	scans := 0
	for _, r := range repos {
		name := strings.ToLower(r.FullName)
		listed[name] = true
		if scanned[name].PushedAt == r.PushedAt || scans == maxScansPerCycle {
			continue
		}
		scans++
		if err := a.scanRepo(ctx, u, token, name, r, scanned[name].SHA); err != nil {
			a.log.Error("repo scan failed", "user", u.ID, "repo", name, "err", err)
		}
	}
	for name := range scanned {
		if listed[name] {
			continue
		}
		if _, err := a.SyncCodeRefs(ctx, u, name, nil); err != nil {
			return err
		}
		if err := a.db.DeleteCodeRepo(ctx, u.ID, name); err != nil {
			return err
		}
	}
	return nil
}

func (a *App) scanRepo(ctx context.Context, u *User, token, name string, r ghRepo, lastSHA string) error {
	sha, err := a.gh.BranchSHA(ctx, token, name, r.DefaultBranch)
	var se *ghStatusError
	if errors.As(err, &se) && (se.Code == 409 || se.Code == 403) {
		sha, err = "", nil // empty repo, or one the token can't read
	}
	if err != nil {
		return err
	}
	if sha != "" && sha != lastSHA {
		body, err := a.gh.Tarball(ctx, token, name, sha)
		if err != nil {
			return err
		}
		refs, err := scanTarball(body, name, sha)
		body.Close()
		if err != nil {
			return err
		}
		failed, err := a.SyncCodeRefs(ctx, u, name, refs)
		if err != nil {
			return err
		}
		for _, e := range failed {
			a.log.Warn("code ref not watched", "user", u.ID, "repo", name, "err", e)
			// Refs to items that don't exist wait for the next push; anything else
			// (rate limits, outages) rescans next cycle.
			if !errors.Is(e, errGHNotFound) {
				return nil
			}
		}
	}
	return a.db.SaveCodeRepo(ctx, u.ID, name, r.PushedAt, sha)
}

// SyncCodeRefs watches everything refs point at and makes refs the only code
// refs from repo. A ref that can't be watched keeps that watch's current refs
// from repo, so a GitHub outage doesn't drop watches. It returns an error
// for each ref that couldn't be watched.
func (a *App) SyncCodeRefs(ctx context.Context, u *User, repo string, refs []CodeRef) (failed []error, err error) {
	watchIDs := map[string]int64{} // lowercase ref -> watch id, 0 if it failed
	var keep []int64
	var resolved []CodeRef
	for _, c := range refs {
		key := strings.ToLower(c.Target.String())
		id, seen := watchIDs[key]
		if !seen {
			wt, _, err := a.AddWatch(ctx, u, c.Target, "", u.DefaultFilter, u.DefaultDelivery)
			if err != nil {
				failed = append(failed, fmt.Errorf("%s:%d: %s: %w", c.Path, c.Line, c.Target, err))
				if it, err := a.db.ItemByRef(ctx, c.Target.Owner, c.Target.Repo, c.Target.Number); err == nil {
					if wt, err := a.db.UserWatchForItem(ctx, u.ID, it.ID); err == nil {
						keep = append(keep, wt.ID)
					}
				}
			} else {
				id = wt.ID
			}
			watchIDs[key] = id
		}
		if id != 0 {
			c.WatchID = id
			resolved = append(resolved, c)
		}
	}
	return failed, a.db.ReplaceCodeRefs(ctx, u.ID, repo, resolved, keep)
}
