package main

import (
	"fmt"
	"html"
	"net/http"
	"sort"
	"strings"
)

func (s *Store) ui(w http.ResponseWriter, q *http.Request, p []string) {
	w.Header().Set("content-type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'")
	var b strings.Builder
	b.WriteString(`<!doctype html><meta charset="utf-8"><title>Go Forge — object observatory</title><style>body{background:#0a1220;color:#cad8eb;max-width:1000px;margin:60px auto;font:16px system-ui}a{color:#55d5c3}pre{padding:24px;background:#142236;overflow:auto}h1{font-size:44px}small{color:#8fa3be}</style><small>GO FORGE / IN-MEMORY OBJECT OBSERVATORY</small><h1>Code at memory speed.</h1>`)
	if len(p) < 2 {
		s.RLock()
		keys := []string{}
		for k, r := range s.Repos {
			if !r.Private {
				keys = append(keys, k)
			}
		}
		s.RUnlock()
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Fprintf(&b, "<p><a href=\"/%s\">%s</a></p>", html.EscapeString(k), html.EscapeString(k))
		}
	} else {
		r := s.repo(p[0], p[1])
		r.RLock()
		defer r.RUnlock()
		if r.Private {
			fail(404, "Not Found")
		}
		fmt.Fprintf(&b, "<h2>%s / %s</h2><p>%d immutable objects · revision %d</p>", html.EscapeString(r.Owner), html.EscapeString(r.Name), len(r.Objects), r.Version)
		files := r.files("main")
		if len(p) >= 5 && p[2] == "blob" {
			path := strings.Join(p[4:], "/")
			e, ok := r.files(p[3])[path]
			if !ok {
				fail(404, "File not found")
			}
			fmt.Fprintf(&b, "<h3>%s</h3><pre>%s</pre>", html.EscapeString(path), html.EscapeString(string(r.Objects[e.SHA].Data)))
		} else {
			names := []string{}
			for name := range files {
				names = append(names, name)
			}
			sort.Strings(names)
			for _, name := range names {
				fmt.Fprintf(&b, "<p><a href=\"/%s/%s/blob/main/%s\">%s</a></p>", html.EscapeString(r.Owner), html.EscapeString(r.Name), html.EscapeString(name), html.EscapeString(name))
			}
			b.WriteString("<h3>Commit history</h3>")
			id := r.resolve("main")
			for count := 0; id != "" && count < 30; count++ {
				o := r.Objects[id]
				_, parents := info(o)
				msg := strings.SplitN(string(o.Data), "\n\n", 2)
				if len(msg) > 1 {
					fmt.Fprintf(&b, "<p><code>%s</code> %s</p>", id[:12], html.EscapeString(msg[1]))
				}
				if len(parents) == 0 {
					break
				}
				id = parents[0]
			}
			b.WriteString("<h3>Issues</h3>")
			for _, v := range r.Issues {
				fmt.Fprintf(&b, "<p>#%v %s · %s</p>", v["number"], html.EscapeString(str(v, "title")), html.EscapeString(str(v, "state")))
			}
		}
	}
	w.Write([]byte(b.String()))
}
