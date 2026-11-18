package main

import (
	"fmt"
	"html"
	"net/http"
	"net/url"
	"sort"
	"strings"
)

type browserLink struct {
	Text string
	URL  string
}

type browserCommit struct {
	ShortID string
	Title   string
}

type browserIssue struct {
	Number int
	Title  string
	State  string
}

type browserPage struct {
	Title        string
	Description  string
	Repositories []browserLink
	Files        []browserLink
	Commits      []browserCommit
	Issues       []browserIssue
	Repository   bool
	ShowContents bool
	Contents     string
}

func renderBrowser(w http.ResponseWriter, page browserPage) {
	var body strings.Builder
	escape := html.EscapeString
	body.WriteString(`<!doctype html><html lang="en"><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">`)
	fmt.Fprintf(&body, "<title>%s · Go Forge</title>", escape(page.Title))
	body.WriteString(`<style>body{max-width:60rem;margin:2rem auto;padding:0 1rem;font:1rem system-ui}pre{padding:1rem;border:1px solid;overflow:auto}li{margin:.4rem 0}</style>`)
	body.WriteString(`<header><a href="/">Go Forge</a></header><main>`)
	fmt.Fprintf(&body, "<h1>%s</h1>", escape(page.Title))
	if page.Description != "" {
		fmt.Fprintf(&body, "<p>%s</p>", escape(page.Description))
	}
	switch {
	case page.ShowContents:
		fmt.Fprintf(&body, "<pre>%s</pre>", escape(page.Contents))
	case page.Repository:
		body.WriteString("<h2>Files</h2>")
		renderLinks(&body, page.Files, "No files.")
		body.WriteString("<h2>Recent commits</h2><ol>")
		for _, commit := range page.Commits {
			fmt.Fprintf(&body, "<li><code>%s</code> %s</li>", escape(commit.ShortID), escape(commit.Title))
		}
		body.WriteString("</ol><h2>Issues</h2><ul>")
		for _, issue := range page.Issues {
			fmt.Fprintf(&body, "<li>#%d %s (%s)</li>", issue.Number, escape(issue.Title), escape(issue.State))
		}
		body.WriteString("</ul>")
	default:
		renderLinks(&body, page.Repositories, "No public repositories.")
	}
	body.WriteString("</main></html>")
	w.Write([]byte(body.String()))
}

func renderLinks(body *strings.Builder, links []browserLink, empty string) {
	body.WriteString("<ul>")
	if len(links) == 0 {
		fmt.Fprintf(body, "<li>%s</li>", html.EscapeString(empty))
	}
	for _, link := range links {
		fmt.Fprintf(body, "<li><a href=\"%s\">%s</a></li>", html.EscapeString(link.URL), html.EscapeString(link.Text))
	}
	body.WriteString("</ul>")
}

func (s *Store) ui(w http.ResponseWriter, q *http.Request, segments []string) {
	if q.Method != http.MethodGet && q.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		fail(http.StatusMethodNotAllowed, "Method not allowed")
	}
	var page browserPage
	if len(segments) < 2 {
		page = s.publicIndex()
	} else {
		repository := s.repo(segments[0], segments[1])
		page = repository.browserView(segments[2:])
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'")
	renderBrowser(w, page)
}

func (s *Store) publicIndex() browserPage {
	page := browserPage{Title: "Public repositories"}
	s.RLock()
	defer s.RUnlock()
	for _, repository := range s.Repos {
		repository.RLock()
		if !repository.Private {
			path := browserPath(repository.Owner, repository.Name)
			label := repository.Owner + "/" + repository.Name
			page.Repositories = append(page.Repositories, browserLink{Text: label, URL: path})
		}
		repository.RUnlock()
	}
	sort.Slice(page.Repositories, func(a, b int) bool {
		return page.Repositories[a].Text < page.Repositories[b].Text
	})
	return page
}

func browserPath(segments ...string) string {
	escaped := make([]string, len(segments))
	for i, part := range segments {
		escaped[i] = url.PathEscape(part)
	}
	return "/" + strings.Join(escaped, "/")
}

func (r *Repo) browserView(route []string) browserPage {
	r.RLock()
	defer r.RUnlock()
	if r.Private {
		fail(404, "Not Found")
	}
	if len(route) >= 3 && route[0] == "blob" {
		filename := strings.Join(route[2:], "/")
		entry, found := r.files(route[1])[filename]
		if !found {
			fail(404, "File not found")
		}
		return browserPage{Title: filename, ShowContents: true, Contents: string(r.Objects[entry.SHA].Data)}
	}
	if len(route) != 0 {
		fail(404, "Not Found")
	}
	page := browserPage{Title: r.Owner + "/" + r.Name, Description: r.Description, Repository: true}
	if tip := r.Refs["refs/heads/main"]; tip != "" {
		fileList := r.files(tip)
		for filename := range fileList {
			parts := append([]string{r.Owner, r.Name, "blob", "main"}, strings.Split(filename, "/")...)
			page.Files = append(page.Files, browserLink{Text: filename, URL: browserPath(parts...)})
		}
		sort.Slice(page.Files, func(a, b int) bool {
			return page.Files[a].Text < page.Files[b].Text
		})
		for current := tip; current != "" && len(page.Commits) < 30; {
			commit, found := r.Objects[current]
			if !found || commit.Type != "commit" {
				break
			}
			_, message, _ := strings.Cut(string(commit.Data), "\n\n")
			title, _, _ := strings.Cut(message, "\n")
			page.Commits = append(page.Commits, browserCommit{ShortID: current[:min(len(current), 12)], Title: title})
			_, parents := info(commit)
			if len(parents) == 0 {
				break
			}
			current = parents[0]
		}
	}
	for _, issue := range r.Issues {
		item := browserIssue{Number: integer(issue["number"]), Title: str(issue, "title"), State: str(issue, "state")}
		page.Issues = append(page.Issues, item)
	}
	return page
}
