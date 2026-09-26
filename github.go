package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Categories a watch can filter on, in the order the UI shows them.
var categories = []struct{ Key, Label string }{
	{"comments", "New comments"},
	{"reviews", "Reviews & approvals"},
	{"commits", "Commits pushed"},
	{"labels", "Labels / assignees"},
	{"state", "Merged / closed / reopened"},
	{"other", "Other (references, renames)"},
}

const (
	filterEverything = "comments,reviews,commits,labels,state,other"
	filterStatusOnly = "state"
)

var ghURLRe = regexp.MustCompile(`(?i)(?:https?://)?(?:www\.)?github\.com/([\w.-]+)/([\w.-]+)/(issues|pull|pulls)/(\d+)`)
var ghShortRe = regexp.MustCompile(`^([\w.-]+)/([\w.-]+)#(\d+)$`)

type Ref struct {
	Owner, Repo string
	Number      int
}

func (r Ref) String() string { return fmt.Sprintf("%s/%s#%d", r.Owner, r.Repo, r.Number) }

// parseRef finds a GitHub issue or PR in free text: a URL (anywhere in the
// text, so shared "title — url" snippets work) or owner/repo#123.
func parseRef(s string) (Ref, bool) {
	s = strings.TrimSpace(s)
	if m := ghURLRe.FindStringSubmatch(s); m != nil {
		n, _ := strconv.Atoi(m[4])
		return Ref{m[1], m[2], n}, n > 0
	}
	if m := ghShortRe.FindStringSubmatch(s); m != nil {
		n, _ := strconv.Atoi(m[3])
		return Ref{m[1], m[2], n}, n > 0
	}
	return Ref{}, false
}

type GitHub struct {
	BaseURL  string // https://api.github.com
	AppToken string
	HTTP     *http.Client

	mu          sync.Mutex
	blockedTill time.Time
}

var (
	errGHNotFound    = errors.New("not found on GitHub (or it's private)")
	errGHRateLimited = errors.New("GitHub rate limit reached")
)

type ghStatusError struct {
	Code int
	Body string
}

func (e *ghStatusError) Error() string { return fmt.Sprintf("GitHub API %d: %s", e.Code, e.Body) }

// get performs a GET. It returns notModified=true for 304 responses.
func (g *GitHub) get(ctx context.Context, path, token, etag string, out any) (newETag string, next bool, notModified bool, err error) {
	g.mu.Lock()
	blocked := time.Now().Before(g.blockedTill)
	g.mu.Unlock()
	if blocked {
		return "", false, false, errGHRateLimited
	}
	req, err := http.NewRequestWithContext(ctx, "GET", g.BaseURL+path, nil)
	if err != nil {
		return "", false, false, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "watchnote")
	if token == "" {
		token = g.AppToken
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	resp, err := g.HTTP.Do(req)
	if err != nil {
		return "", false, false, err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotModified:
		return etag, false, true, nil
	case resp.StatusCode == http.StatusNotFound:
		return "", false, false, errGHNotFound
	case resp.StatusCode == 429 || (resp.StatusCode == 403 && resp.Header.Get("X-RateLimit-Remaining") == "0"):
		reset := time.Now().Add(time.Minute)
		if s, err := strconv.ParseInt(resp.Header.Get("X-RateLimit-Reset"), 10, 64); err == nil {
			reset = time.Unix(s, 0)
		} else if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil {
			reset = time.Now().Add(time.Duration(s) * time.Second)
		}
		// Only the shared app token blocks everything; user tokens have their own quota.
		if token == g.AppToken {
			g.mu.Lock()
			g.blockedTill = reset
			g.mu.Unlock()
		}
		return "", false, false, errGHRateLimited
	case resp.StatusCode >= 300:
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return "", false, false, &ghStatusError{resp.StatusCode, strings.TrimSpace(string(b))}
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return "", false, false, fmt.Errorf("decode %s: %w", path, err)
	}
	return resp.Header.Get("ETag"), strings.Contains(resp.Header.Get("Link"), `rel="next"`), false, nil
}

type ghUser struct {
	Login string `json:"login"`
}

// ghIssue covers both /issues/N and /pulls/N responses.
type ghIssue struct {
	Title       string    `json:"title"`
	State       string    `json:"state"`
	HTMLURL     string    `json:"html_url"`
	User        ghUser    `json:"user"`
	Comments    int       `json:"comments"`
	UpdatedAt   time.Time `json:"updated_at"`
	MergedAt    *string   `json:"merged_at"`        // pulls endpoint
	MergeSHA    string    `json:"merge_commit_sha"` // pulls endpoint
	PullRequest *struct {
		MergedAt *string `json:"merged_at"`
	} `json:"pull_request"` // issues endpoint, present for PRs
}

type Snapshot struct {
	Kind, Title, State, Author, HTMLURL string
	MergeSHA                            string
	Comments                            int
	UpdatedAt                           time.Time
}

// FetchItem loads an issue or PR. For PRs it uses the pulls endpoint, whose
// ETag also changes when commits are pushed.
func (g *GitHub) FetchItem(ctx context.Context, r Ref, kind, token, etag string) (*Snapshot, string, bool, error) {
	path := fmt.Sprintf("/repos/%s/%s/issues/%d", url.PathEscape(r.Owner), url.PathEscape(r.Repo), r.Number)
	if kind == "pr" {
		path = fmt.Sprintf("/repos/%s/%s/pulls/%d", url.PathEscape(r.Owner), url.PathEscape(r.Repo), r.Number)
	}
	var is ghIssue
	newETag, _, notModified, err := g.get(ctx, path, token, etag, &is)
	if err != nil || notModified {
		return nil, newETag, notModified, err
	}
	s := &Snapshot{Kind: "issue", Title: is.Title, State: is.State, Author: is.User.Login, HTMLURL: is.HTMLURL,
		Comments: is.Comments, UpdatedAt: is.UpdatedAt}
	if kind == "pr" || is.PullRequest != nil {
		s.Kind = "pr"
		if is.MergedAt != nil || (is.PullRequest != nil && is.PullRequest.MergedAt != nil) {
			s.State = "merged"
			s.MergeSHA = is.MergeSHA
		}
	}
	if kind == "" && s.Kind == "pr" {
		// First fetch went through /issues; the ETag belongs to that endpoint, so drop it.
		newETag = ""
	}
	return s, newETag, false, nil
}

func (g *GitHub) Timeline(ctx context.Context, r Ref, token string, page int) ([]json.RawMessage, bool, error) {
	var evs []json.RawMessage
	path := fmt.Sprintf("/repos/%s/%s/issues/%d/timeline?per_page=100&page=%d",
		url.PathEscape(r.Owner), url.PathEscape(r.Repo), r.Number, page)
	_, next, _, err := g.get(ctx, path, token, "", &evs)
	return evs, next, err
}

type ghRelease struct {
	TagName     string    `json:"tag_name"`
	HTMLURL     string    `json:"html_url"`
	PublishedAt time.Time `json:"published_at"`
}

// LatestRelease returns the repo's latest release, or nil if it has none.
func (g *GitHub) LatestRelease(ctx context.Context, r Ref, token, etag string) (*ghRelease, string, bool, error) {
	var rel ghRelease
	path := fmt.Sprintf("/repos/%s/%s/releases/latest", url.PathEscape(r.Owner), url.PathEscape(r.Repo))
	newETag, _, notModified, err := g.get(ctx, path, token, etag, &rel)
	if errors.Is(err, errGHNotFound) {
		return nil, "", false, nil
	}
	if err != nil || notModified {
		return nil, newETag, notModified, err
	}
	return &rel, newETag, false, nil
}

// Contains reports whether sha is reachable from ref.
func (g *GitHub) Contains(ctx context.Context, r Ref, ref, sha, token string) (bool, error) {
	var cmp struct {
		Status string `json:"status"`
	}
	path := fmt.Sprintf("/repos/%s/%s/compare/%s...%s?per_page=1",
		url.PathEscape(r.Owner), url.PathEscape(r.Repo), url.PathEscape(ref), url.PathEscape(sha))
	_, _, _, err := g.get(ctx, path, token, "", &cmp)
	return cmp.Status == "behind" || cmp.Status == "identical", err
}

func (g *GitHub) Viewer(ctx context.Context, token string) (string, error) {
	var u ghUser
	_, _, _, err := g.get(ctx, "/user", token, "", &u)
	return u.Login, err
}

type ghRepo struct {
	FullName      string `json:"full_name"`
	DefaultBranch string `json:"default_branch"`
	PushedAt      string `json:"pushed_at"`
	Private       bool   `json:"private"`
	Fork          bool   `json:"fork"`
	Permissions   struct {
		Push bool `json:"push"`
	} `json:"permissions"`
}

// scannable reports whether Watchnote scans the repo's code: its user can
// push to it, and it isn't a fork.
func (r ghRepo) scannable() bool { return r.Permissions.Push && !r.Fork }

// UserRepos lists the repos token can see.
func (g *GitHub) UserRepos(ctx context.Context, token string) ([]ghRepo, error) {
	var out []ghRepo
	for page := 1; page <= 20; page++ {
		var rs []ghRepo
		_, next, _, err := g.get(ctx, fmt.Sprintf("/user/repos?per_page=100&page=%d", page), token, "", &rs)
		if err != nil {
			return nil, err
		}
		out = append(out, rs...)
		if !next {
			break
		}
	}
	return out, nil
}

// CanSee reports whether token covers repo at all, as opposed to covering it
// without some permission.
func (g *GitHub) CanSee(ctx context.Context, token, repo string) (bool, error) {
	var r ghRepo
	_, _, _, err := g.get(ctx, "/repos/"+repo, token, "", &r)
	var se *ghStatusError
	if errors.Is(err, errGHNotFound) || errors.As(err, &se) && se.Code == 403 {
		return false, nil
	}
	return err == nil, err
}

func (g *GitHub) BranchSHA(ctx context.Context, token, repo, branch string) (string, error) {
	var ref struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	_, _, _, err := g.get(ctx, "/repos/"+repo+"/git/ref/heads/"+url.PathEscape(branch), token, "", &ref)
	return ref.Object.SHA, err
}

// Tarball downloads repo at sha as a gzipped tarball.
func (g *GitHub) Tarball(ctx context.Context, token, repo, sha string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", g.BaseURL+"/repos/"+repo+"/tarball/"+sha, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "watchnote")
	req.Header.Set("Authorization", "Bearer "+token)
	client := *g.HTTP
	client.Timeout = 5 * time.Minute // big repos take longer than API calls
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 {
		resp.Body.Close()
		return nil, &ghStatusError{resp.StatusCode, "tarball"}
	}
	return resp.Body, nil
}

// ---- timeline event classification ----

type ghTimelineEvent struct {
	Event  string  `json:"event"`
	ID     int64   `json:"id"`
	SHA    string  `json:"sha"`
	Actor  *ghUser `json:"actor"`
	User   *ghUser `json:"user"`
	Author *struct {
		Name string    `json:"name"`
		Date time.Time `json:"date"`
	} `json:"author"`
	Body        string     `json:"body"`
	Message     string     `json:"message"`
	HTMLURL     string     `json:"html_url"`
	State       string     `json:"state"`
	StateReason string     `json:"state_reason"`
	CommitID    string     `json:"commit_id"`
	CreatedAt   *time.Time `json:"created_at"`
	SubmittedAt *time.Time `json:"submitted_at"`
	Label       *struct {
		Name string `json:"name"`
	} `json:"label"`
	Assignee          *ghUser `json:"assignee"`
	RequestedReviewer *ghUser `json:"requested_reviewer"`
	RequestedTeam     *struct {
		Name string `json:"name"`
	} `json:"requested_team"`
	Milestone *struct {
		Title string `json:"title"`
	} `json:"milestone"`
	Rename *struct {
		From string `json:"from"`
		To   string `json:"to"`
	} `json:"rename"`
	Source *struct {
		Issue *struct {
			ID         int64  `json:"id"`
			Number     int    `json:"number"`
			Title      string `json:"title"`
			HTMLURL    string `json:"html_url"`
			Repository *struct {
				FullName string `json:"full_name"`
			} `json:"repository"`
		} `json:"issue"`
	} `json:"source"`
	Comments []struct {
		ID      int64  `json:"id"`
		Body    string `json:"body"`
		HTMLURL string `json:"html_url"`
		User    ghUser `json:"user"`
	} `json:"comments"`
}

// Events that are noise for watchers.
var ignoredEvents = map[string]bool{
	"subscribed": true, "unsubscribed": true, "mentioned": true, "referenced": true,
	"head_ref_deleted": true, "head_ref_restored": true, "comment_deleted": true,
	"added_to_project": true, "moved_columns_in_project": true,
	"removed_from_project": true, "converted_note_to_issue": true, "automatic_base_change_succeeded": true,
	"automatic_base_change_failed": true, "user_blocked": true, "auto_merge_enabled": true,
	"auto_merge_disabled": true, "auto_squash_enabled": true, "auto_rebase_enabled": true,
	"added_to_merge_queue": true, "removed_from_merge_queue": true, "deployed": true,
	"deployment_environment_changed": true,
}

// classify turns a raw timeline event into an Event. ok=false means skip it.
func classify(raw json.RawMessage, fallbackTime time.Time) (e Event, ok bool) {
	var t ghTimelineEvent
	if err := json.Unmarshal(raw, &t); err != nil || t.Event == "" || ignoredEvents[t.Event] {
		return e, false
	}
	actor := ""
	switch {
	case t.Actor != nil:
		actor = t.Actor.Login
	case t.User != nil:
		actor = t.User.Login
	}
	e.Kind = t.Event
	e.Actor = actor
	e.URL = t.HTMLURL
	e.OccurredAt = fallbackTime.Unix()
	switch {
	case t.CreatedAt != nil:
		e.OccurredAt = t.CreatedAt.Unix()
	case t.SubmittedAt != nil:
		e.OccurredAt = t.SubmittedAt.Unix()
	case t.Author != nil && !t.Author.Date.IsZero():
		e.OccurredAt = t.Author.Date.Unix()
	}
	switch {
	case t.ID != 0:
		e.GHKey = fmt.Sprintf("%s:%d", t.Event, t.ID)
	case t.SHA != "":
		e.GHKey = "commit:" + t.SHA
	case len(t.Comments) > 0:
		e.GHKey = fmt.Sprintf("%s:%d", t.Event, t.Comments[0].ID)
	case t.Source != nil && t.Source.Issue != nil:
		e.GHKey = fmt.Sprintf("%s:%d", t.Event, t.Source.Issue.ID)
	default:
		sum := sha256.Sum256(raw)
		e.GHKey = t.Event + ":" + hex.EncodeToString(sum[:8])
	}

	switch t.Event {
	case "commented":
		e.Category = "comments"
		e.Summary = actor + " commented"
		e.Body = excerpt(t.Body)
	case "reviewed":
		e.Category = "reviews"
		switch strings.ToLower(t.State) {
		case "approved":
			e.Summary = actor + " approved these changes"
		case "changes_requested":
			e.Summary = actor + " requested changes"
		case "dismissed":
			e.Summary = actor + "'s review was dismissed"
		default:
			e.Summary = actor + " reviewed"
		}
		e.Body = excerpt(t.Body)
	case "line-commented":
		e.Category = "reviews"
		if len(t.Comments) > 0 {
			c := t.Comments[0]
			e.Actor = c.User.Login
			e.URL = c.HTMLURL
			e.Summary = fmt.Sprintf("%s left %s on the code", c.User.Login, plural(len(t.Comments), "review comment"))
			e.Body = excerpt(c.Body)
		} else {
			e.Summary = "New review comments"
		}
	case "review_requested", "review_request_removed":
		e.Category = "reviews"
		who := "someone"
		if t.RequestedReviewer != nil {
			who = t.RequestedReviewer.Login
		} else if t.RequestedTeam != nil {
			who = "team " + t.RequestedTeam.Name
		}
		if t.Event == "review_requested" {
			e.Summary = fmt.Sprintf("%s requested a review from %s", actor, who)
		} else {
			e.Summary = fmt.Sprintf("%s removed the review request for %s", actor, who)
		}
	case "review_dismissed":
		e.Category = "reviews"
		e.Summary = actor + " dismissed a review"
	case "committed":
		e.Category = "commits"
		if t.Author != nil {
			e.Actor = t.Author.Name
		}
		e.Summary = "Commit pushed: " + firstLine(t.Message)
	case "head_ref_force_pushed":
		e.Category = "commits"
		e.Summary = actor + " force-pushed the branch"
	case "labeled", "unlabeled":
		e.Category = "labels"
		name := ""
		if t.Label != nil {
			name = t.Label.Name
		}
		verb := map[string]string{"labeled": "added", "unlabeled": "removed"}[t.Event]
		e.Summary = fmt.Sprintf("%s %s label %s", actor, verb, name)
	case "assigned", "unassigned":
		e.Category = "labels"
		who := ""
		if t.Assignee != nil {
			who = t.Assignee.Login
		}
		if t.Event == "assigned" {
			e.Summary = fmt.Sprintf("%s assigned %s", actor, who)
		} else {
			e.Summary = fmt.Sprintf("%s unassigned %s", actor, who)
		}
	case "milestoned", "demilestoned":
		e.Category = "labels"
		title := ""
		if t.Milestone != nil {
			title = t.Milestone.Title
		}
		if t.Event == "milestoned" {
			e.Summary = fmt.Sprintf("%s added this to milestone %s", actor, title)
		} else {
			e.Summary = fmt.Sprintf("%s removed this from milestone %s", actor, title)
		}
	case "closed":
		e.Category = "state"
		switch {
		case t.StateReason == "not_planned":
			e.Summary = actor + " closed this as not planned"
		case t.CommitID != "":
			e.Summary = fmt.Sprintf("%s closed this in commit %.7s", actor, t.CommitID)
		case t.StateReason == "completed":
			e.Summary = actor + " closed this as completed"
		default:
			e.Summary = actor + " closed this"
		}
	case "reopened":
		e.Category = "state"
		e.Summary = actor + " reopened this"
	case "merged":
		e.Category = "state"
		e.Summary = actor + " merged this"
		if t.CommitID != "" {
			e.Summary += fmt.Sprintf(" (%.7s)", t.CommitID)
		}
	case "ready_for_review":
		e.Category = "state"
		e.Summary = actor + " marked this ready for review"
	case "convert_to_draft":
		e.Category = "state"
		e.Summary = actor + " converted this to a draft"
	case "cross-referenced":
		e.Category = "other"
		if t.Source != nil && t.Source.Issue != nil {
			si := t.Source.Issue
			repo := ""
			if si.Repository != nil {
				repo = si.Repository.FullName
			}
			e.Summary = fmt.Sprintf("%s mentioned this in %s#%d: %s", actor, repo, si.Number, si.Title)
			e.URL = si.HTMLURL
		} else {
			e.Summary = actor + " mentioned this elsewhere"
		}
	case "renamed":
		e.Category = "other"
		if t.Rename != nil {
			e.Summary = fmt.Sprintf("%s renamed this to %q", actor, t.Rename.To)
		} else {
			e.Summary = actor + " renamed this"
		}
	case "connected":
		e.Category = "other"
		e.Summary = actor + " linked a pull request"
	case "disconnected":
		e.Category = "other"
		e.Summary = actor + " unlinked a pull request"
	default:
		e.Category = "other"
		e.Summary = strings.TrimSpace(actor + " " + strings.ReplaceAll(t.Event, "_", " "))
	}
	return e, true
}

func excerpt(s string) string {
	s = strings.TrimSpace(s)
	const max = 600
	if r := []rune(s); len(r) > max {
		return strings.TrimSpace(string(r[:max])) + "…"
	}
	return s
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	return s
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}
