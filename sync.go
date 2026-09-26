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
	"time"
)

// Code comments can hold watches: a comment on a line of its own like
//
//	// octo/hello#7: We need this until the upstream fix ships.
//
// keeps octo/hello#7 watched for as long as the comment is on the default
// branch. An added line in a patch file (`+  // octo/hello#7: …`) counts too.
// Watchnote scans the repos a user's saved GitHub tokens can push to, again
// whenever they're pushed to.

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

var codeRefRe = regexp.MustCompile(`^[\s\p{P}\p{S}]*?([\w.-]+)/([\w.-]+)#(\d+):\s+(\S.*?)\s*(?:\*/|-->|\*\)|-\}|"""|''')?\s*$`)

const (
	maxScanFile      = 1 << 20
	maxScansPerCycle = 10 // per user; the rest wait for the next cycle
)

// parseCodeRef reports whether line is a reference comment.
func parseCodeRef(line string) (target Ref, note string, ok bool) {
	if strings.HasPrefix(line, "-") && !strings.HasPrefix(line, "--") {
		return Ref{}, "", false // removed by a patch
	}
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
// with a GitHub token, and drops refs from repos no token reaches any more.
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

// listTokenRepos asks GitHub which repos t can see and saves the answer. When
// GitHub doesn't answer, it saves why and t keeps its last list.
func (a *App) listTokenRepos(ctx context.Context, t *GitHubToken) (token string, repos []ghRepo, err error) {
	if token, err = a.keys.Open(t.Sealed); err != nil {
		return "", nil, err
	}
	if repos, err = a.gh.UserRepos(ctx, token); err != nil {
		t.ListError = err.Error()
		return "", nil, errors.Join(err, a.db.SetTokenListError(ctx, t.ID, t.ListError))
	}
	t.Repos, t.ListedAt, t.ListError = nil, a.now().Unix(), ""
	for _, r := range repos {
		t.Repos = append(t.Repos, TokenRepo{Name: r.FullName, Private: r.Private, Scan: r.scannable()})
	}
	return token, repos, a.db.SetTokenRepos(ctx, t.ID, t.Repos, t.ListedAt)
}

// scanSoon scans u's code in the background, as after a token is added or
// fixed, so Settings shows what it can read without waiting for the scanner.
func (a *App) scanSoon(u *User) {
	a.bg.Add(1)
	go func() {
		defer a.bg.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		if err := a.scanUserCode(ctx, u); err != nil {
			a.log.Error("code scan failed", "user", u.ID, "err", err)
		}
	}()
}

func (a *App) scanUserCode(ctx context.Context, u *User) error {
	a.scanMu.Lock()
	defer a.scanMu.Unlock()
	tokens, err := a.db.GitHubTokens(ctx, u.ID)
	if err != nil {
		return err
	}
	scanned, err := a.db.CodeRepos(ctx, u.ID)
	if err != nil {
		return err
	}
	listed := map[string]bool{} // repos to keep refs from
	var order []string
	found := map[string]*listedRepo{}
	for _, t := range tokens {
		token, repos, err := a.listTokenRepos(ctx, t)
		if err != nil {
			// Hold on to refs from the repos it listed before, so a GitHub outage
			// doesn't drop watches.
			a.log.Error("listing a token's repos failed", "user", u.ID, "token", t.ID, "err", err)
			for _, r := range t.Scanned() {
				listed[r.Key()] = true
			}
			continue
		}
		for _, r := range repos {
			name := strings.ToLower(r.FullName)
			if !r.scannable() {
				continue
			}
			listed[name] = true
			if found[name] == nil {
				found[name] = &listedRepo{repo: r}
				order = append(order, name)
			}
			found[name].tokens = append(found[name].tokens, repoToken{t.ID, token})
		}
	}
	scans := 0
	for _, name := range order {
		lr, prev := found[name], scanned[name]
		if prev.PushedAt == lr.repo.PushedAt && !lr.untried(prev, a.now()) {
			continue
		}
		if scans == maxScansPerCycle {
			break
		}
		scans++
		if err := a.scanRepo(ctx, u, name, lr, prev); err != nil {
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

// listedRepo is a repo to scan and the tokens that list it, in the order
// they're tried. A fine-grained token lists every repo its user can push to
// but reads only its own owner's private ones, so the first may be refused.
type listedRepo struct {
	repo   ghRepo
	tokens []repoToken
}

type repoToken struct {
	id    int64
	token string
}

// refusalRetry is how long a token GitHub refused a repo's code to isn't
// asked for it again, unless the repo is pushed to or the user asks sooner.
const refusalRetry = 24 * time.Hour

// refused reports whether GitHub refused token id the repo recently.
func (r CodeRepo) refused(id int64, now time.Time) bool {
	f, ok := r.Refusals[id]
	return ok && now.Sub(time.Unix(f.At, 0)) < refusalRetry
}

// untried reports whether the repo was last left unread and a token that
// lists it hasn't been refused it lately, like one added or fixed since.
func (lr *listedRepo) untried(prev CodeRepo, now time.Time) bool {
	if prev.TokenID != 0 || prev.SHA != "" {
		return false
	}
	for _, t := range lr.tokens {
		if !prev.refused(t.id, now) {
			return true
		}
	}
	return false
}

func (a *App) scanRepo(ctx context.Context, u *User, name string, lr *listedRepo, prev CodeRepo) error {
	now := a.now()
	saved := CodeRepo{PushedAt: lr.repo.PushedAt, Refusals: map[int64]Refusal{}}
	for id := range prev.Refusals {
		if prev.PushedAt == lr.repo.PushedAt && prev.refused(id, now) {
			saved.Refusals[id] = prev.Refusals[id]
		}
	}
	for _, t := range lr.tokens {
		if saved.refused(t.id, now) {
			continue
		}
		sha, err := a.gh.BranchSHA(ctx, t.token, name, lr.repo.DefaultBranch)
		var se *ghStatusError
		if errors.As(err, &se) && se.Code == 403 {
			// GitHub says 403 both when the token lacks the Contents permission
			// and when it doesn't cover the repo; whether it sees the repo tells
			// which, and so what the user should fix.
			sees, err := a.gh.CanSee(ctx, t.token, name)
			if err != nil {
				return err
			}
			why := refusedAccess
			if sees {
				why = refusedContents
			}
			saved.Refusals[t.id] = Refusal{Why: why, At: now.Unix()}
			continue
		}
		if errors.As(err, &se) && se.Code == 409 {
			sha, err = "", nil // empty repo
		}
		if err != nil {
			return err
		}
		saved.SHA, saved.TokenID = sha, t.id
		if sha != "" && sha != prev.SHA {
			body, err := a.gh.Tarball(ctx, t.token, name, sha)
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
		break
	}
	// A repo no token can read keeps the refs it had, and is tried again after
	// its next push, once another token lists it, or a day on.
	return a.db.SaveCodeRepo(ctx, u.ID, name, saved)
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
