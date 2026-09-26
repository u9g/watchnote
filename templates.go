package main

import (
	"bytes"
	"embed"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"strings"
	texttemplate "text/template"
	"time"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/renderer"
	"github.com/yuin/goldmark/util"
)

//go:embed templates static
var assets embed.FS

// Goldmark escapes raw HTML and drops unsafe link schemes by default, so its
// output is safe to mark as template.HTML.
var md = goldmark.New(goldmark.WithExtensions(extension.GFM))

// mdPreview renders links as their text, for notes shown inside a link.
var mdPreview = goldmark.New(goldmark.WithExtensions(extension.GFM),
	goldmark.WithRendererOptions(renderer.WithNodeRenderers(util.Prioritized(linkText{}, 0))))

type linkText struct{}

func (linkText) RegisterFuncs(r renderer.NodeRendererFuncRegisterer) {
	r.Register(ast.KindLink, func(util.BufWriter, []byte, ast.Node, bool) (ast.WalkStatus, error) {
		return ast.WalkContinue, nil
	})
	r.Register(ast.KindAutoLink, func(w util.BufWriter, src []byte, n ast.Node, entering bool) (ast.WalkStatus, error) {
		if entering {
			w.Write(util.EscapeHTML(n.(*ast.AutoLink).Label(src)))
		}
		return ast.WalkContinue, nil
	})
}

func renderMarkdown(m goldmark.Markdown, s string) template.HTML {
	var b bytes.Buffer
	if err := m.Convert([]byte(s), &b); err != nil {
		return template.HTML(template.HTMLEscapeString(s))
	}
	return template.HTML(b.String())
}

var funcs = map[string]any{
	"markdown":        func(s string) template.HTML { return renderMarkdown(md, s) },
	"markdownPreview": func(s string) template.HTML { return renderMarkdown(mdPreview, s) },
	"ago": func(unix int64) string {
		if unix == 0 {
			return "never"
		}
		d := time.Since(time.Unix(unix, 0))
		switch {
		case d < time.Minute:
			return "just now"
		case d < time.Hour:
			return fmt.Sprintf("%dm ago", int(d.Minutes()))
		case d < 24*time.Hour:
			return fmt.Sprintf("%dh ago", int(d.Hours()))
		case d < 30*24*time.Hour:
			return fmt.Sprintf("%dd ago", int(d.Hours()/24))
		default:
			return time.Unix(unix, 0).Format("Jan 2, 2006")
		}
	},
	"stateLabel": func(it *Item) string {
		kind := "Issue"
		if it.Kind == "pr" {
			kind = "PR"
		}
		return map[string]string{"open": "Open", "closed": "Closed", "merged": "Merged"}[it.State] + " " + kind
	},
	"categories": func() any { return categories },
	"inFilter":   inFilter,
	"isNew":      func(e *Event, since int64) bool { return e.ID > since },
	"minutes": func(v any) string {
		var m int64
		switch x := v.(type) {
		case int64:
			m = x
		case int:
			m = int64(x)
		}
		return fmt.Sprintf("%02d:%02d", m/60, m%60)
	},
	"codeRefExamples": func() any { return codeRefExamples },
	"hours": func() []int {
		h := make([]int, 24)
		for i := range h {
			h[i] = i
		}
		return h
	},
	"join": strings.Join,
	"list": func(v ...any) []any { return v },
	"plural": func(n int, one, many string) string {
		if n == 1 {
			return one
		}
		return many
	},
	"hourLabel": func(h int) string { return time.Date(2000, 1, 1, h, 0, 0, 0, time.UTC).Format("3 PM") },
	"initial": func(s string) string {
		if s == "" {
			return "?"
		}
		return strings.ToUpper(string([]rune(s)[:1]))
	},
}

type pageTemplate struct{ *template.Template }

// loadPages parses each page together with the shared layout, so every page
// can define its own "content" block.
func loadPages() (map[string]*pageTemplate, error) {
	base, err := template.New("").Funcs(funcs).ParseFS(assets, "templates/layout.html", "templates/partials.html")
	if err != nil {
		return nil, err
	}
	names, err := fs.Glob(assets, "templates/page_*.html")
	if err != nil {
		return nil, err
	}
	pages := map[string]*pageTemplate{}
	for _, n := range names {
		t, err := template.Must(base.Clone()).ParseFS(assets, n)
		if err != nil {
			return nil, err
		}
		key := strings.TrimSuffix(strings.TrimPrefix(n, "templates/page_"), ".html")
		pages[key] = &pageTemplate{t}
	}
	return pages, nil
}

func (a *App) render(w http.ResponseWriter, status int, page string, d *pageData) {
	var buf bytes.Buffer
	if err := a.pages[page].ExecuteTemplate(&buf, "layout", d); err != nil {
		a.log.Error("render", "page", page, "err", err)
		http.Error(w, "internal error", 500)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	buf.WriteTo(w)
}

var (
	emailHTML = template.Must(template.New("").Funcs(funcs).ParseFS(assets, "templates/email/*.html"))
	emailText = texttemplate.Must(texttemplate.New("").Funcs(funcs).ParseFS(assets, "templates/email/*.txt"))
)

func renderEmail(name string, data any) (html, text string, err error) {
	var h, t bytes.Buffer
	if err := emailHTML.ExecuteTemplate(&h, name+".html", data); err != nil {
		return "", "", err
	}
	if err := emailText.ExecuteTemplate(&t, name+".txt", data); err != nil {
		return "", "", err
	}
	return h.String(), t.String(), nil
}
