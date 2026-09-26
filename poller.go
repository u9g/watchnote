package main

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// pollDue polls every item whose next_poll_at has passed.
func (a *App) pollDue(ctx context.Context) error {
	items, err := a.db.DueItems(ctx, a.now(), 50)
	if err != nil {
		return err
	}
	for _, it := range items {
		if ctx.Err() != nil {
			return nil
		}
		if err := a.pollItem(ctx, it); err != nil {
			a.log.Error("poll failed", "item", it.Ref(), "err", err)
		}
	}
	return nil
}

func refOf(it *Item) Ref { return Ref{it.Owner, it.Repo, it.Number} }

// itemToken picks the GitHub token to read an item with: the app token for
// public repos, a watcher's own token for private ones.
func (a *App) itemToken(ctx context.Context, it *Item) (string, error) {
	if !it.Private {
		return "", nil
	}
	tokens, err := a.db.TokenCandidates(ctx, it)
	if err != nil {
		return "", err
	}
	for _, t := range tokens {
		if tok, err := a.keys.Open(t.Sealed); err == nil {
			return tok, nil
		}
	}
	return "", errors.New("private repo, and no watcher has a GitHub token saved")
}

func (a *App) pollItem(ctx context.Context, it *Item) error {
	now := a.now()
	fail := func(err error, retry time.Duration) error {
		it.LastError = err.Error()
		it.NextPollAt = now.Add(retry).Unix()
		if serr := a.db.SaveItem(ctx, it); serr != nil {
			return serr
		}
		return err
	}

	token, err := a.itemToken(ctx, it)
	if err != nil {
		return fail(err, time.Hour)
	}
	etag := it.ETag
	if it.State == "merged" && it.MergeSHA == "" {
		etag = "" // merged before merge_sha was stored
	}
	snap, etag, notModified, err := a.gh.FetchItem(ctx, refOf(it), it.Kind, token, etag)
	switch {
	case errors.Is(err, errGHRateLimited):
		return fail(err, 5*time.Minute)
	case errors.Is(err, errGHNotFound):
		return fail(err, time.Hour)
	case err != nil:
		return fail(err, 10*time.Minute)
	}
	if !notModified {
		it.ETag = etag
		applySnapshot(it, snap)
	}

	// The ETag covers the item itself; activity like a new review may not touch
	// it, so re-read the timeline at least hourly regardless.
	fresh := 0
	if !notModified || now.Sub(time.Unix(it.LastTimeline, 0)) > time.Hour {
		fresh, err = a.syncTimeline(ctx, it, token, now)
		if err != nil {
			return fail(err, 10*time.Minute)
		}
	}
	if it.State == "merged" && it.MergeSHA != "" && it.ReleasedIn == "" {
		released, err := a.checkRelease(ctx, it, token, now)
		if err != nil {
			return fail(err, 10*time.Minute)
		}
		if released {
			fresh++
		}
	}
	if fresh > 0 {
		it.LastActivity = now.Unix()
	}
	it.LastError = ""
	it.NextPollAt = now.Add(pollInterval(it, now)).Unix()
	return a.db.SaveItem(ctx, it)
}

// checkRelease records a "released" event once the repo's latest release
// contains the PR's merge commit.
func (a *App) checkRelease(ctx context.Context, it *Item, token string, now time.Time) (bool, error) {
	rel, etag, notModified, err := a.gh.LatestRelease(ctx, refOf(it), token, it.ReleaseETag)
	if err != nil || notModified || rel == nil {
		return false, err
	}
	ok, err := a.gh.Contains(ctx, refOf(it), rel.TagName, it.MergeSHA, token)
	if err != nil {
		return false, err
	}
	it.ReleaseETag = etag
	if !ok {
		return false, nil
	}
	it.ReleasedIn = rel.TagName
	e := Event{ItemID: it.ID, GHKey: "released:" + rel.TagName, Kind: "released", Category: "state",
		Summary: "Released in " + rel.TagName, URL: rel.HTMLURL, OccurredAt: rel.PublishedAt.Unix(), IngestedAt: now.Unix()}
	return a.db.InsertEvent(ctx, &e)
}

func applySnapshot(it *Item, s *Snapshot) {
	it.Kind = s.Kind
	it.Title = s.Title
	it.State = s.State
	it.Author = s.Author
	it.Comments = s.Comments
	if s.HTMLURL != "" {
		it.HTMLURL = s.HTMLURL
	}
	it.GHUpdatedAt = s.UpdatedAt.Unix()
	if s.MergeSHA != "" {
		it.MergeSHA = s.MergeSHA
	}
}

// pollInterval backs off for quiet items. 304s are free against the rate
// limit, so active items can be polled often.
func pollInterval(it *Item, now time.Time) time.Duration {
	last := time.Unix(max(it.LastActivity, it.GHUpdatedAt), 0)
	quiet := now.Sub(last)
	switch {
	case it.State != "open":
		return time.Hour
	case quiet < 24*time.Hour:
		return 2 * time.Minute
	case quiet < 7*24*time.Hour:
		return 10 * time.Minute
	default:
		return time.Hour
	}
}

// syncTimeline reads timeline pages starting from the last page seen and
// stores events not seen before. It returns how many were new.
func (a *App) syncTimeline(ctx context.Context, it *Item, token string, now time.Time) (int, error) {
	page := max(1, it.TimelinePage)
	backedOff := false
	fresh := 0
	for n := 0; n < 50; n++ {
		evs, next, err := a.gh.Timeline(ctx, refOf(it), token, page)
		if err != nil {
			return fresh, err
		}
		// Deleted events can shrink the timeline so our last page no longer exists.
		if len(evs) == 0 && page > 1 && !backedOff {
			page--
			backedOff = true
			continue
		}
		for _, raw := range evs {
			e, ok := classify(raw, now)
			if !ok {
				continue
			}
			e.ItemID = it.ID
			e.IngestedAt = now.Unix()
			isNew, err := a.db.InsertEvent(ctx, &e)
			if err != nil {
				return fresh, err
			}
			if isNew {
				fresh++
			}
		}
		if !next {
			break
		}
		page++
	}
	it.TimelinePage = page
	it.LastTimeline = now.Unix()
	return fresh, nil
}

// ---- adding watches ----

// fetchSnapshot reads an item not yet in the database, falling back to the
// user's own GitHub tokens for private repos.
func (a *App) fetchSnapshot(ctx context.Context, u *User, r Ref) (snap *Snapshot, etag string, private bool, token string, err error) {
	snap, etag, _, err = a.gh.FetchItem(ctx, r, "", "", "")
	if !errors.Is(err, errGHNotFound) {
		return snap, etag, false, "", err
	}
	tokens, terr := a.db.GitHubTokensFor(ctx, u.ID, r.Owner+"/"+r.Repo)
	if terr != nil {
		return nil, "", false, "", terr
	}
	for _, t := range tokens {
		if token, err = a.keys.Open(t.Sealed); err != nil {
			return nil, "", false, "", err
		}
		snap, etag, _, err = a.gh.FetchItem(ctx, r, "", token, "")
		if !errors.Is(err, errGHNotFound) {
			return snap, etag, true, token, err
		}
	}
	return nil, "", false, "", err
}

// AddWatch starts watching r for u, for a code comment that references it.
// If u already watches it, the existing watch is returned with created=false
// and nothing changes.
func (a *App) AddWatch(ctx context.Context, u *User, r Ref, filter, delivery string) (w *Watch, created bool, err error) {
	it, err := a.db.ItemByRef(ctx, r.Owner, r.Repo, r.Number)
	if errors.Is(err, errNotFound) {
		it, err = a.createItem(ctx, u, r)
	}
	if err != nil {
		return nil, false, err
	}
	if w, err := a.db.UserWatchForItem(ctx, u.ID, it.ID); err == nil {
		return w, false, nil
	}
	// Everything already on the timeline counts as seen.
	maxID, err := a.db.MaxEventID(ctx, it.ID)
	if err != nil {
		return nil, false, err
	}
	w = &Watch{UserID: u.ID, ItemID: it.ID, Filter: filter, Delivery: delivery,
		NotifiedEventID: maxID, ViewedEventID: maxID, CreatedAt: a.now().Unix()}
	if err := a.db.InsertWatch(ctx, w); err != nil {
		return nil, false, err
	}
	if it.Private && !it.TokenUserID.Valid {
		it.TokenUserID = sql.NullInt64{Int64: u.ID, Valid: true}
		if err := a.db.SaveItem(ctx, it); err != nil {
			return nil, false, err
		}
	}
	w, err = a.db.WatchByID(ctx, w.ID)
	return w, true, err
}

func (a *App) createItem(ctx context.Context, u *User, r Ref) (*Item, error) {
	snap, etag, private, token, err := a.fetchSnapshot(ctx, u, r)
	if err != nil {
		return nil, err
	}
	now := a.now()
	it := &Item{Owner: r.Owner, Repo: r.Repo, Number: r.Number, ETag: etag, Private: private}
	if private {
		it.TokenUserID = sql.NullInt64{Int64: u.ID, Valid: true}
	}
	applySnapshot(it, snap)
	it.NextPollAt = now.Add(pollInterval(it, now)).Unix()
	if err := a.db.InsertItem(ctx, it); err != nil {
		// Someone else added it at the same moment.
		if existing, err2 := a.db.ItemByRef(ctx, r.Owner, r.Repo, r.Number); err2 == nil {
			return existing, nil
		}
		return nil, err
	}
	it.Owner, it.Repo = strings.ToLower(it.Owner), strings.ToLower(it.Repo)
	// Seed the timeline so existing history isn't emailed as new. A half-seeded
	// item would email old history later, so drop it and try again next scan.
	if _, err := a.syncTimeline(ctx, it, token, now); err != nil {
		a.db.ExecContext(ctx, `DELETE FROM items WHERE id = ?`, it.ID)
		return nil, err
	}
	return it, a.db.SaveItem(ctx, it)
}
