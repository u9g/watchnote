package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"net/smtp"
	"net/textproto"
	"sort"
	"strconv"
	"strings"
	"time"
)

type Message struct {
	To      string
	Subject string
	Text    string
	HTML    string
	Headers map[string]string
}

type Sender interface {
	Send(ctx context.Context, from string, m *Message) error
}

const (
	batchQuiet = 60 * time.Second // wait for a burst of events to settle...
	batchMax   = 5 * time.Minute  // ...but never hold an email longer than this
)

// ---- deciding what to send ----

func inFilter(filter, category string) bool {
	for _, c := range strings.Split(filter, ",") {
		if c == category {
			return true
		}
	}
	return false
}

func matching(evs []*Event, filter string) []*Event {
	var out []*Event
	for _, e := range evs {
		if inFilter(filter, e.Category) {
			out = append(out, e)
		}
	}
	return out
}

func userLocation(u *User) *time.Location {
	if loc, err := time.LoadLocation(u.TZ); err == nil && u.TZ != "" {
		return loc
	}
	return time.UTC
}

func inQuietHours(u *User, now time.Time) bool {
	if !u.QuietStart.Valid || !u.QuietEnd.Valid || u.QuietStart.Int64 == u.QuietEnd.Int64 {
		return false
	}
	t := now.In(userLocation(u))
	m := int64(t.Hour()*60 + t.Minute())
	s, e := u.QuietStart.Int64, u.QuietEnd.Int64
	if s < e {
		return m >= s && m < e
	}
	return m >= s || m < e // wraps midnight
}

// readyToSend reports whether a batch of pending events has settled.
func readyToSend(evs []*Event, now time.Time) bool {
	oldest, newest := evs[0].IngestedAt, evs[0].IngestedAt
	for _, e := range evs {
		oldest = min(oldest, e.IngestedAt)
		newest = max(newest, e.IngestedAt)
	}
	return now.Sub(time.Unix(newest, 0)) >= batchQuiet || now.Sub(time.Unix(oldest, 0)) >= batchMax
}

type pendingWatch struct {
	w     *Watch
	evs   []*Event
	maxID int64
}

func (a *App) runMailer(ctx context.Context) error {
	now := a.now()
	watches, err := a.db.WatchesWithPending(ctx)
	if err != nil {
		return err
	}
	users := map[int64]*User{}
	digests := map[int64][]pendingWatch{}
	for _, w := range watches {
		u, ok := users[w.UserID]
		if !ok {
			if u, err = a.db.UserByID(ctx, w.UserID); err != nil {
				return err
			}
			users[w.UserID] = u
		}
		all, err := a.db.EventsAfter(ctx, w.ItemID, w.NotifiedEventID)
		if err != nil || len(all) == 0 {
			continue
		}
		maxID := all[len(all)-1].ID
		skip := func() error { return a.db.SetWatchNotified(ctx, w.ID, maxID) }

		wasDone := false
		switch w.Status {
		case "muted":
			if err := skip(); err != nil {
				return err
			}
			continue
		case "done":
			wasDone = true
			// Done items stay quiet unless they're reopened, which puts them back in Active.
			reopened := false
			for _, e := range all {
				reopened = reopened || e.Kind == "reopened"
			}
			if !reopened {
				if err := skip(); err != nil {
					return err
				}
				continue
			}
			if err := a.db.SetWatchStatus(ctx, w.ID, "active"); err != nil {
				return err
			}
			w.Status = "active"
		}

		evs := matching(all, w.Filter)
		if wasDone && len(evs) == 0 {
			for _, e := range all {
				if e.Kind == "reopened" {
					evs = append(evs, e) // always tell people a done item came back
				}
			}
		}
		if len(evs) == 0 {
			if err := skip(); err != nil {
				return err
			}
			continue
		}
		if w.Delivery == "digest" {
			digests[u.ID] = append(digests[u.ID], pendingWatch{w, evs, maxID})
			continue
		}
		if !readyToSend(all, now) || inQuietHours(u, now) {
			continue
		}
		if err := a.sendActivity(ctx, u, w, evs); err != nil {
			a.log.Error("send failed", "user", u.ID, "watch", w.ID, "err", err)
			continue
		}
		if err := skip(); err != nil {
			return err
		}
	}

	for uid, pws := range digests {
		u := users[uid]
		local := now.In(userLocation(u))
		day := local.Format("2006-01-02")
		if local.Hour() < u.DigestHour || u.LastDigestDay == day {
			continue
		}
		if err := a.sendDigest(ctx, u, pws); err != nil {
			a.log.Error("digest failed", "user", u.ID, "err", err)
			continue
		}
		for _, pw := range pws {
			if err := a.db.SetWatchNotified(ctx, pw.w.ID, pw.maxID); err != nil {
				return err
			}
		}
		if err := a.db.SetLastDigestDay(ctx, u.ID, day); err != nil {
			return err
		}
	}
	return nil
}

// ---- composing ----

var categoryRank = map[string]int{"state": 0, "reviews": 1, "comments": 2, "commits": 3, "labels": 4, "other": 5}

// headline picks the most important event: state changes first, then reviews, and so on; newest wins ties.
func headline(evs []*Event) *Event {
	best := evs[0]
	for _, e := range evs[1:] {
		if r, rb := categoryRank[e.Category], categoryRank[best.Category]; r < rb || (r == rb && e.ID > best.ID) {
			best = e
		}
	}
	return best
}

func subjectFor(it *Item, evs []*Event) string {
	h := headline(evs)
	var s string
	switch h.Kind {
	case "merged":
		s = "✅ Merged"
	case "closed":
		s = "Closed"
		if strings.Contains(h.Summary, "not planned") {
			s = "Closed as not planned"
		}
	case "reopened":
		s = "Reopened"
	case "reviewed":
		switch {
		case strings.Contains(h.Summary, "approved"):
			s = "New review: approved"
		case strings.Contains(h.Summary, "requested changes"):
			s = "New review: changes requested"
		default:
			s = "New review from " + h.Actor
		}
	case "commented":
		s = "New comment from " + h.Actor
	case "committed":
		n := 0
		for _, e := range evs {
			if e.Kind == "committed" {
				n++
			}
		}
		s = plural(n, "new commit")
	default:
		s = h.Summary
	}
	if len(evs) > 1 {
		s += fmt.Sprintf(" (+%d more)", len(evs)-1)
	}
	return fmt.Sprintf("[%s] %s", it.Ref(), s)
}

type emailLinks struct {
	GitHub, Watch, Settings, Mute, StatusOnly, Done string
}

func (a *App) linksFor(w *Watch, githubURL string) emailLinks {
	signed := func(action string) string {
		return fmt.Sprintf("%s/e/%s?w=%d&s=%s", a.cfg.BaseURL, action, w.ID, a.keys.LinkSig(action, w.ID))
	}
	return emailLinks{
		GitHub:     githubURL,
		Watch:      fmt.Sprintf("%s/items/%d", a.cfg.BaseURL, w.ID),
		Settings:   a.cfg.BaseURL + "/settings",
		Mute:       signed("mute"),
		StatusOnly: signed("statusonly"),
		Done:       signed("done"),
	}
}

type activityEmail struct {
	Item     *Item
	Watch    *Watch
	Since    string
	Headline *Event
	Others   []*Event
	Resolved bool // merged or closed: ask if they're done
	Links    emailLinks
	BaseURL  string
}

func (a *App) sendActivity(ctx context.Context, u *User, w *Watch, evs []*Event) error {
	h := headline(evs)
	var others []*Event
	for _, e := range evs {
		if e != h {
			others = append(others, e)
		}
	}
	link := h.URL
	if link == "" {
		link = w.Item.HTMLURL
	}
	data := activityEmail{
		Item: w.Item, Watch: w, Headline: h, Others: others,
		Since:    sinceLabel(w, u, a.now()),
		Resolved: h.Kind == "merged" || h.Kind == "closed" || h.Kind == "released",
		Links:    a.linksFor(w, link),
		BaseURL:  a.cfg.BaseURL,
	}
	html, text, err := renderEmail("activity", data)
	if err != nil {
		return err
	}
	m := &Message{To: u.Email, Subject: subjectFor(w.Item, evs), HTML: html, Text: text, Headers: a.threadHeaders(w, data.Links.Mute)}
	return a.mail.Send(ctx, a.cfg.MailFrom, m)
}

type digestEmail struct {
	Entries []digestEntry
	BaseURL string
}

type digestEntry struct {
	Item   *Item
	Watch  *Watch
	Since  string
	Events []*Event
	Links  emailLinks
}

func (a *App) sendDigest(ctx context.Context, u *User, pws []pendingWatch) error {
	sort.Slice(pws, func(i, j int) bool { return pws[i].w.Item.Ref() < pws[j].w.Item.Ref() })
	data := digestEmail{BaseURL: a.cfg.BaseURL}
	for _, pw := range pws {
		data.Entries = append(data.Entries, digestEntry{
			Item: pw.w.Item, Watch: pw.w, Since: sinceLabel(pw.w, u, a.now()), Events: pw.evs,
			Links: a.linksFor(pw.w, pw.w.Item.HTMLURL),
		})
	}
	html, text, err := renderEmail("digest", data)
	if err != nil {
		return err
	}
	m := &Message{
		To:      u.Email,
		Subject: fmt.Sprintf("Watchnote daily digest: %s updated", plural(len(pws), "item")),
		HTML:    html, Text: text,
		Headers: map[string]string{"Message-ID": a.messageID("digest")},
	}
	return a.mail.Send(ctx, a.cfg.MailFrom, m)
}

func sinceLabel(w *Watch, u *User, now time.Time) string {
	t := time.Unix(w.CreatedAt, 0).In(userLocation(u))
	s := "Watched since " + t.Format("Jan 2, 2006")
	if days := int(now.Sub(t).Hours() / 24); days >= 7 {
		s += fmt.Sprintf(" · %d days ago", days)
	}
	return s
}

func (a *App) mailDomain() string {
	if addr, err := mail.ParseAddress(a.cfg.MailFrom); err == nil {
		if _, d, ok := strings.Cut(addr.Address, "@"); ok {
			return d
		}
	}
	return "watchnote.local"
}

func (a *App) messageID(kind string) string {
	return fmt.Sprintf("<%s.%s@%s>", kind, randomToken(12), a.mailDomain())
}

// threadHeaders point every email about a watch at one root message, which
// clients that thread by References (Apple Mail, Thunderbird, Outlook) group
// together. Unsubscribing mutes the watch; only deleting the comment stops it.
func (a *App) threadHeaders(w *Watch, unsubscribe string) map[string]string {
	root := fmt.Sprintf("<watch-%d@%s>", w.ID, a.mailDomain())
	return map[string]string{
		"Message-ID":            a.messageID("event"),
		"In-Reply-To":           root,
		"References":            root,
		"List-Unsubscribe":      "<" + unsubscribe + ">",
		"List-Unsubscribe-Post": "List-Unsubscribe=One-Click",
		"List-ID":               fmt.Sprintf("%s <%s.%s.watchnote>", w.Item.Ref(), w.Item.Repo, w.Item.Owner),
	}
}

// ---- MIME + transport ----

func buildMIME(from string, m *Message, now time.Time) ([]byte, error) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	h := func(k, v string) { fmt.Fprintf(&buf, "%s: %s\r\n", k, v) }
	h("From", from)
	h("To", m.To)
	h("Subject", mime.QEncoding.Encode("utf-8", strings.NewReplacer("\r", " ", "\n", " ").Replace(m.Subject)))
	h("Date", now.Format(time.RFC1123Z))
	h("MIME-Version", "1.0")
	keys := make([]string, 0, len(m.Headers))
	for k := range m.Headers {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		h(k, m.Headers[k])
	}
	h("Content-Type", `multipart/alternative; boundary="`+w.Boundary()+`"`)
	buf.WriteString("\r\n")
	for _, part := range []struct{ typ, body string }{{"text/plain", m.Text}, {"text/html", m.HTML}} {
		pw, err := w.CreatePart(textproto.MIMEHeader{
			"Content-Type":              {part.typ + "; charset=utf-8"},
			"Content-Transfer-Encoding": {"quoted-printable"},
		})
		if err != nil {
			return nil, err
		}
		qp := quotedprintable.NewWriter(pw)
		qp.Write([]byte(part.body))
		qp.Close()
	}
	w.Close()
	return buf.Bytes(), nil
}

type logSender struct{ Sent []*Message }

func (l *logSender) Send(_ context.Context, from string, m *Message) error {
	l.Sent = append(l.Sent, m)
	slog.Info("email (MAIL_MODE=log)", "from", from, "to", m.To, "subject", m.Subject, "body", m.Text)
	return nil
}

type smtpSender struct {
	Host               string
	Port               int
	Username, Password string
}

func (s *smtpSender) Send(ctx context.Context, from string, m *Message) error {
	fromAddr, err := mail.ParseAddress(from)
	if err != nil {
		return fmt.Errorf("MAIL_FROM: %w", err)
	}
	msg, err := buildMIME(from, m, time.Now())
	if err != nil {
		return err
	}
	addr := net.JoinHostPort(s.Host, strconv.Itoa(s.Port))
	d := net.Dialer{Timeout: 20 * time.Second}
	var conn net.Conn
	if s.Port == 465 { // implicit TLS
		conn, err = tls.DialWithDialer(&d, "tcp", addr, &tls.Config{ServerName: s.Host})
	} else {
		conn, err = d.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return err
	}
	conn.SetDeadline(time.Now().Add(time.Minute))
	c, err := smtp.NewClient(conn, s.Host)
	if err != nil {
		conn.Close()
		return err
	}
	defer c.Close()
	if ok, _ := c.Extension("STARTTLS"); ok && s.Port != 465 {
		if err := c.StartTLS(&tls.Config{ServerName: s.Host}); err != nil {
			return err
		}
	}
	if s.Username != "" {
		if err := c.Auth(smtp.PlainAuth("", s.Username, s.Password, s.Host)); err != nil {
			return err
		}
	}
	if err := c.Mail(fromAddr.Address); err != nil {
		return err
	}
	if err := c.Rcpt(m.To); err != nil {
		return err
	}
	wc, err := c.Data()
	if err != nil {
		return err
	}
	if _, err := wc.Write(msg); err != nil {
		return err
	}
	if err := wc.Close(); err != nil {
		return err
	}
	return c.Quit()
}
