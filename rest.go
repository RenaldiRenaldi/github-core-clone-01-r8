package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

func (s *Store) api(w http.ResponseWriter, q *http.Request, p []string, in M) {
	method := q.Method
	if len(p) == 0 || p[0] == "" {
		w.Header().Set("X-OAuth-Scopes", "repo, read:org")
		j(w, M{"current_user_url": "/api/v3/user"})
		return
	}
	if p[0] == "meta" {
		j(w, M{"installed_version": "go-forge-4"})
		return
	}
	if p[0] == "user" && len(p) == 1 {
		j(w, M{"login": "adaiversen", "id": 1, "name": "Ada Iversen"})
		return
	}
	if (p[0] == "user" || p[0] == "users" || p[0] == "orgs") && p[len(p)-1] == "repos" {
		owner := "adaiversen"
		if p[0] != "user" {
			owner = p[1]
		}
		if method == "POST" {
			r := s.create(owner, str(in, "name"))
			r.Description = str(in, "description")
			r.Private = in["private"] == true
			if in["auto_init"] == true {
				blob := r.put("blob", []byte("# "+r.Name+"\n"))
				r.Refs["refs/heads/main"] = r.commit(r.tree(map[string]Entry{"README.md": {"100644", blob}}), nil, "Initial commit")
			}
			answer(w, 201, r.json())
			return
		}
		items := []M{}
		s.RLock()
		for _, r := range s.Repos {
			if r.Owner == owner {
				r.RLock()
				items = append(items, r.json())
				r.RUnlock()
			}
		}
		s.RUnlock()
		sort.Slice(items, func(i, j int) bool { return integer(items[i]["id"]) < integer(items[j]["id"]) })
		page, _ := strconv.Atoi(q.URL.Query().Get("page"))
		if page < 1 {
			page = 1
		}
		limit, _ := strconv.Atoi(q.URL.Query().Get("per_page"))
		if limit < 1 {
			limit = 30
		}
		start := (page - 1) * limit
		if start > len(items) {
			start = len(items)
		}
		end := start + limit
		if end < len(items) {
			w.Header().Set("Link", fmt.Sprintf("<%s?page=%d&per_page=%d>; rel=\"next\"", "http://"+q.Host+q.URL.Path, page+1, limit))
		} else {
			end = len(items)
		}
		j(w, items[start:end])
		return
	}
	if p[0] == "search" {
		items := []M{}
		s.RLock()
		for _, r := range s.Repos {
			r.RLock()
			for _, issue := range r.Issues {
				if strings.Contains(strings.ToLower(str(issue, "title")), strings.ToLower(q.URL.Query().Get("q"))) {
					items = append(items, issue)
				}
			}
			r.RUnlock()
		}
		s.RUnlock()
		j(w, M{"total_count": len(items), "incomplete_results": false, "items": items})
		return
	}
	if len(p) < 3 || p[0] != "repos" {
		fail(404, "Not Found")
	}
	r := s.repo(p[1], p[2])
	if len(p) == 3 && method == "DELETE" {
		s.Lock()
		delete(s.Repos, p[1]+"/"+p[2])
		s.Unlock()
		answer(w, 204, nil)
		return
	}
	r.Lock()
	defer r.Unlock()
	if len(p) == 3 {
		switch method {
		case "GET":
			data := r.Cache["repo"]
			if data == nil {
				data, _ = json.Marshal(r.json())
				r.Cache["repo"] = data
			}
			w.Header().Set("content-type", "application/json")
			w.Write(data)
		case "PATCH":
			if v, ok := in["description"].(string); ok {
				r.Description = v
			}
			r.touch()
			j(w, r.json())
		case "DELETE":
			s.Lock()
			delete(s.Repos, p[1]+"/"+p[2])
			s.Unlock()
			answer(w, 204, nil)
		default:
			fail(405, "Method not allowed")
		}
		return
	}
	route := p[3]
	tail := p[4:]
	switch route {
	case "contents":
		path := strings.Join(tail, "/")
		branch := str(in, "branch")
		if branch == "" {
			branch = q.URL.Query().Get("ref")
		}
		if branch == "" {
			branch = "main"
		}
		files := r.files(branch)
		e, exists := files[path]
		if method == "GET" {
			if !exists {
				entries := []M{}
				for name, item := range files {
					if path == "" || strings.HasPrefix(name, path+"/") {
						entries = append(entries, M{"name": name[strings.LastIndex(name, "/")+1:], "path": name, "sha": item.SHA, "type": "file", "size": len(r.Objects[item.SHA].Data)})
					}
				}
				if len(entries) == 0 {
					fail(404, "File not found")
				}
				j(w, entries)
				return
			}
			o := r.Objects[e.SHA]
			if strings.Contains(q.Header.Get("Accept"), "raw") {
				w.Header().Set("content-type", "application/octet-stream")
				w.Write(o.Data)
				return
			}
			j(w, M{"name": path, "path": path, "sha": e.SHA, "size": len(o.Data), "type": "file", "encoding": "base64", "content": base64.StdEncoding.EncodeToString(o.Data)})
			return
		}
		if exists && str(in, "sha") != e.SHA {
			fail(409, "Stale file SHA")
		}
		if method == "DELETE" {
			if !exists {
				fail(404, "File not found")
			}
			delete(files, path)
		} else {
			d, err := base64.StdEncoding.DecodeString(str(in, "content"))
			if err != nil {
				fail(422, "Invalid base64")
			}
			files[path] = Entry{"100644", r.put("blob", d)}
		}
		parent := r.resolve(branch)
		id := r.commit(r.tree(files), []string{parent}, str(in, "message"))
		r.Refs["refs/heads/"+branch] = id
		r.touch()
		code := 200
		if !exists {
			code = 201
		}
		answer(w, code, M{"content": M{"sha": files[path].SHA, "path": path, "encoding": "base64"}, "commit": M{"sha": id}})
		return
	case "labels":
		if method == "POST" {
			name := str(in, "name")
			if name == "" {
				fail(422, "name required")
			}
			r.Labels[name] = in
			r.touch()
			answer(w, 201, in)
			return
		}
		out := []M{}
		for _, v := range r.Labels {
			out = append(out, v)
		}
		j(w, out)
		return
	case "issues", "pulls":
		pull := route == "pulls"
		list := r.Issues
		if pull {
			list = r.PRs
		}
		if len(tail) == 0 {
			if method == "POST" {
				var v M
				if pull {
					v = r.newPull(in)
					v = r.pullJSON(v)
				} else {
					v = r.newIssue(in)
				}
				answer(w, 201, v)
				return
			}
			key := "list:" + route + ":" + q.URL.RawQuery
			if cached := r.Cache[key]; cached != nil {
				w.Header().Set("content-type", "application/json")
				w.Write(cached)
				return
			}
			out := []M{}
			for _, v := range list {
				state := q.URL.Query().Get("state")
				if state == "" {
					state = "open"
				}
				if state != "all" && v["state"] != state {
					continue
				}
				if pull {
					v = r.pullJSON(v)
				}
				out = append(out, v)
			}
			if q.URL.Query().Get("direction") != "asc" {
				sort.Slice(out, func(i, j int) bool { return integer(out[i]["number"]) > integer(out[j]["number"]) })
			}
			b, _ := json.Marshal(out)
			r.Cache[key] = b
			w.Header().Set("content-type", "application/json")
			w.Write(b)
			return
		}
		n := integer(tail[0])
		var item M
		for _, v := range list {
			if integer(v["number"]) == n {
				item = v
			}
		}
		if item == nil {
			fail(404, "Not Found")
		}
		if len(tail) > 1 {
			switch tail[1] {
			case "files":
				j(w, r.diff(r.resolve(str(item, "base_ref")), r.resolve(str(item, "head_ref"))))
				return
			case "merge":
				j(w, r.mergePull(item, in))
				return
			case "reviews":
				reviews := item["reviews"].([]M)
				if len(tail) > 3 && tail[3] == "dismissals" {
					for _, v := range reviews {
						if integer(v["id"]) == integer(tail[2]) {
							v["state"] = "DISMISSED"
							r.touch()
							j(w, v)
							return
						}
					}
				}
				if method == "POST" {
					head := r.resolve(str(item, "head_ref"))
					if sha := str(in, "commit_id"); sha != "" && sha != head {
						fail(409, "Stale review")
					}
					states := map[string]string{"APPROVE": "APPROVED", "REQUEST_CHANGES": "CHANGES_REQUESTED", "COMMENT": "COMMENTED"}
					state := states[str(in, "event")]
					if state == "" {
						fail(422, "Invalid review event")
					}
					v := M{"id": len(reviews) + 1, "state": state, "body": str(in, "body"), "commit_id": head, "user": M{"login": "adaiversen"}}
					item["reviews"] = append(reviews, v)
					r.touch()
					answer(w, 201, v)
					return
				}
				j(w, reviews)
				return
			case "comments":
				comments := item["comments_list"].([]M)
				if method == "POST" {
					v := M{"id": len(comments) + 1, "body": str(in, "body"), "user": M{"login": "adaiversen"}}
					item["comments_list"] = append(comments, v)
					item["comments"] = len(comments) + 1
					r.touch()
					answer(w, 201, v)
					return
				}
				j(w, comments)
				return
			}
		}
		if method == "PATCH" {
			for _, k := range []string{"title", "body", "state"} {
				if v, ok := in[k]; ok {
					item[k] = v
				}
			}
			r.touch()
		}
		if pull {
			j(w, r.pullJSON(item))
		} else {
			j(w, item)
		}
		return
	case "branches", "tags":
		if len(tail) > 1 && tail[1] == "protection" {
			if method == "PUT" {
				r.Protection[tail[0]] = in
				r.touch()
			}
			if method == "DELETE" {
				delete(r.Protection, tail[0])
				answer(w, 204, nil)
				return
			}
			j(w, r.Protection[tail[0]])
			return
		}
		prefix := "refs/heads/"
		if route == "tags" {
			prefix = "refs/tags/"
		}
		out := []M{}
		for _, ref := range refNames(r) {
			if strings.HasPrefix(ref, prefix) {
				v := M{"name": strings.TrimPrefix(ref, prefix), "commit": M{"sha": r.Refs[ref]}, "protected": r.Protection[strings.TrimPrefix(ref, prefix)] != nil}
				if len(tail) > 0 && tail[0] == strings.TrimPrefix(ref, prefix) {
					j(w, v)
					return
				}
				out = append(out, v)
			}
		}
		if len(tail) > 0 {
			fail(404, "Not Found")
		}
		j(w, out)
		return
	case "git":
		r.gitAPI(w, method, tail, in)
		return
	case "commits":
		id := q.URL.Query().Get("sha")
		if id == "" {
			id = "main"
		}
		if len(tail) > 0 {
			id = tail[0]
		}
		id = r.resolve(id)
		out := []M{}
		seen := map[string]bool{}
		queue := []string{id}
		for len(queue) > 0 {
			sha := queue[0]
			queue = queue[1:]
			if sha == "" || seen[sha] {
				continue
			}
			seen[sha] = true
			o := r.Objects[sha]
			tree, parents := info(o)
			v := M{"sha": sha, "commit": M{"message": strings.TrimSuffix(strings.SplitN(string(o.Data), "\n\n", 2)[1], "\n"), "tree": M{"sha": tree}}, "parents": parents}
			if len(tail) > 0 {
				j(w, v)
				return
			}
			out = append(out, v)
			queue = append(queue, parents...)
		}
		j(w, out)
		return
	case "events":
		j(w, []M{})
		return
	}
	fail(404, "Not Found")
}
func (r *Repo) gitAPI(w http.ResponseWriter, method string, p []string, in M) {
	if len(p) == 0 {
		fail(404, "Not Found")
	}
	kind := p[0]
	if kind == "refs" || kind == "ref" {
		ref := "refs/" + strings.Join(p[1:], "/")
		if method == "POST" {
			ref = str(in, "ref")
		}
		if method == "POST" || method == "PATCH" {
			sha := str(in, "sha")
			if !validRef(ref) {
				fail(422, "Invalid ref")
			}
			closure(r.Objects, []string{sha})
			if strings.HasPrefix(ref, "refs/heads/") && r.Objects[sha].Type != "commit" {
				fail(422, "Branch must point to commit")
			}
			r.Refs[ref] = sha
			r.touch()
			answer(w, map[bool]int{true: 201, false: 200}[method == "POST"], M{"ref": ref, "object": M{"sha": sha, "type": r.Objects[sha].Type}})
			return
		}
		if method == "DELETE" {
			delete(r.Refs, ref)
			r.touch()
			answer(w, 204, nil)
			return
		}
		if id := r.Refs[ref]; id != "" {
			j(w, M{"ref": ref, "object": M{"sha": id, "type": r.Objects[id].Type}})
			return
		}
		out := []M{}
		for _, k := range refNames(r) {
			if len(p) == 1 || strings.HasPrefix(k, ref) {
				out = append(out, M{"ref": k, "object": M{"sha": r.Refs[k], "type": r.Objects[r.Refs[k]].Type}})
			}
		}
		j(w, out)
		return
	}
	if kind == "blobs" {
		if method == "POST" {
			data := []byte(str(in, "content"))
			if in["encoding"] == "base64" {
				data, _ = base64.StdEncoding.DecodeString(str(in, "content"))
			}
			id := r.put("blob", data)
			answer(w, 201, M{"sha": id})
			return
		}
		o, ok := r.Objects[p[1]]
		if !ok {
			fail(404, "Unknown blob")
		}
		j(w, M{"sha": p[1], "content": base64.StdEncoding.EncodeToString(o.Data), "encoding": "base64", "size": len(o.Data)})
		return
	}
	if kind == "trees" {
		if method == "POST" {
			files := map[string]Entry{}
			if base := str(in, "base_tree"); base != "" {
				files = r.files(base)
			}
			items, _ := in["tree"].([]any)
			for _, v := range items {
				m := v.(map[string]any)
				sha := str(m, "sha")
				if content, ok := m["content"].(string); ok {
					sha = r.put("blob", []byte(content))
				}
				if sha == "" {
					delete(files, str(m, "path"))
				} else {
					files[str(m, "path")] = Entry{str(m, "mode"), sha}
				}
			}
			answer(w, 201, M{"sha": r.tree(files)})
			return
		}
		files := r.files(p[1])
		out := []M{}
		for path, e := range files {
			out = append(out, M{"path": path, "mode": e.Mode, "type": "blob", "sha": e.SHA, "size": len(r.Objects[e.SHA].Data)})
		}
		sort.Slice(out, func(i, j int) bool { return str(out[i], "path") < str(out[j], "path") })
		j(w, M{"sha": p[1], "tree": out, "truncated": false})
		return
	}
	if kind == "commits" {
		if method == "POST" {
			parents := []string{}
			for _, v := range in["parents"].([]any) {
				parents = append(parents, v.(string))
			}
			id := r.commit(str(in, "tree"), parents, str(in, "message"))
			answer(w, 201, M{"sha": id})
			return
		}
		o, ok := r.Objects[p[1]]
		if !ok {
			fail(404, "Unknown commit")
		}
		tree, ps := info(o)
		parents := []M{}
		for _, id := range ps {
			parents = append(parents, M{"sha": id})
		}
		j(w, M{"sha": p[1], "tree": M{"sha": tree}, "parents": parents, "message": strings.TrimSuffix(strings.SplitN(string(o.Data), "\n\n", 2)[1], "\n")})
		return
	}
	fail(404, "Not Found")
}
