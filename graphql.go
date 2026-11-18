package main

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"unicode"
)

// Small recursive selection parser: aliases, variables, nested inputs and projection.
type field struct {
	name     string
	alias    string
	args     M
	children []field
}
type parser struct {
	tokens []string
	i      int
	vars   M
	depth  int
}

func lex(query string) []string {
	var tokens []string
	for cursor := 0; cursor < len(query); {
		begin := cursor
		switch current := query[cursor]; {
		case current == ',' || unicode.IsSpace(rune(current)):
			cursor++
			continue
		case current == '#':
			newline := strings.IndexAny(query[cursor:], "\r\n")
			if newline < 0 {
				return tokens
			}
			cursor += newline
			continue
		case current == '"':
			cursor = quotedEnd(query, cursor)
		case strings.HasPrefix(query[cursor:], "..."):
			cursor += 3
		case nameByte(current):
			for cursor++; cursor < len(query) && nameByte(query[cursor]); cursor++ {
			}
		default:
			cursor++
		}
		tokens = append(tokens, query[begin:cursor])
	}
	return tokens
}

func nameByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' ||
		c >= '0' && c <= '9' || c == '_' || c == '-'
}

func quotedEnd(query string, open int) int {
	escaped := false
	for end := open + 1; end < len(query); end++ {
		if escaped {
			escaped = false
			continue
		}
		switch query[end] {
		case '\\':
			escaped = true
		case '"':
			return end + 1
		}
	}
	fail(400, "Unterminated GraphQL string")
	return len(query)
}
func (p *parser) peek() string {
	if p.i >= len(p.tokens) {
		return ""
	}
	return p.tokens[p.i]
}
func (p *parser) take() string {
	token := p.peek()
	if token != "" {
		p.i++
	}
	return token
}

func (p *parser) accept(want string) bool {
	if p.peek() != want {
		return false
	}
	p.i++
	return true
}

func (p *parser) require(want string) {
	if !p.accept(want) {
		fail(400, "Expected GraphQL token "+want)
	}
}

func (p *parser) enter() {
	p.depth++
	if p.depth > 64 {
		fail(400, "GraphQL nesting limit exceeded")
	}
}
func (p *parser) value() any {
	p.enter()
	defer func() { p.depth-- }()
	t := p.take()
	if t == "$" {
		return p.vars[p.take()]
	}
	if t == "{" {
		v := M{}
		for p.peek() != "}" && p.peek() != "" {
			k := p.take()
			p.require(":")
			v[k] = p.value()
		}
		p.require("}")
		return v
	}
	if t == "[" {
		v := []any{}
		for p.peek() != "]" && p.peek() != "" {
			v = append(v, p.value())
		}
		p.require("]")
		return v
	}
	if strings.HasPrefix(t, "\"") {
		v, _ := strconv.Unquote(t)
		return v
	}
	if t == "true" {
		return true
	}
	if t == "false" {
		return false
	}
	if t == "null" {
		return nil
	}
	if n, e := strconv.Atoi(t); e == nil {
		return n
	}
	return t
}
func (p *parser) selection() []field {
	p.enter()
	defer func() { p.depth-- }()
	p.require("{")
	var fields []field
	for !p.accept("}") {
		if p.peek() == "" {
			fail(400, "Unterminated GraphQL selection")
		}
		if p.accept("...") {
			if p.accept("on") {
				p.take()
			}
			if p.peek() == "{" {
				fields = append(fields, p.selection()...)
			} else {
				p.take()
			}
			continue
		}
		key := p.take()
		selected := field{name: key, alias: key}
		if p.accept(":") {
			selected.name = p.take()
		}
		if p.accept("(") {
			selected.args = M{}
			for !p.accept(")") {
				arg := p.take()
				p.require(":")
				selected.args[arg] = p.value()
			}
		}
		if p.peek() == "{" {
			selected.children = p.selection()
		}
		fields = append(fields, selected)
	}
	return fields
}
func project(v any, fs []field) any {
	if len(fs) == 0 {
		return v
	}
	switch x := v.(type) {
	case M:
		out := M{}
		for _, f := range fs {
			val := x[f.name]
			if f.name == "__typename" {
				val = "Repository"
			}
			out[f.alias] = project(val, f.children)
		}
		return out
	case []M:
		out := []any{}
		for _, v := range x {
			out = append(out, project(v, fs))
		}
		return out
	case []any:
		out := []any{}
		for _, v := range x {
			out = append(out, project(v, fs))
		}
		return out
	}
	return v
}
func conn(items []M) M {
	edges := []M{}
	for i, v := range items {
		edges = append(edges, M{"node": v, "cursor": fmt.Sprint(i + 1)})
	}
	return M{"nodes": items, "edges": edges, "totalCount": len(items), "pageInfo": M{"hasNextPage": false, "hasPreviousPage": false, "endCursor": fmt.Sprint(len(items)), "startCursor": "1"}}
}
func gqlIssue(v M) M {
	out := M{}
	for k, x := range v {
		out[k] = x
	}
	out["state"] = strings.ToUpper(str(v, "state"))
	out["id"] = v["node_id"]
	out["databaseId"] = v["id"]
	out["author"] = M{"login": "adaiversen"}
	out["labels"] = conn(v["labels"].([]M))
	return out
}
func (s *Store) gqlRepo(r *Repo, fs []field) M {
	out := r.json()
	out["id"] = fmt.Sprintf("Repository:%d", r.ID)
	out["databaseId"] = r.ID
	out["nameWithOwner"] = r.Owner + "/" + r.Name
	out["viewerPermission"] = "ADMIN"
	out["hasIssuesEnabled"] = true
	out["defaultBranchRef"] = M{"name": "main", "target": M{"oid": r.resolve("main")}}
	for _, f := range fs {
		switch f.name {
		case "issues", "pullRequests":
			list := r.Issues
			if f.name == "pullRequests" {
				list = r.PRs
			}
			items := []M{}
			for _, v := range list {
				if states, ok := f.args["states"].([]any); ok && len(states) > 0 {
					match := false
					for _, state := range states {
						if state == strings.ToUpper(str(v, "state")) {
							match = true
						}
					}
					if !match {
						continue
					}
				}
				g := gqlIssue(v)
				if f.name == "pullRequests" {
					g["headRefName"] = v["head_ref"]
					g["baseRefName"] = v["base_ref"]
					g["merged"] = v["merged"]
					g["mergeable"] = "MERGEABLE"
					g["files"] = conn(r.diff(r.resolve(str(v, "base_ref")), r.resolve(str(v, "head_ref"))))
				}
				items = append(items, g)
			}
			if order, ok := f.args["orderBy"].(M); ok && order["direction"] == "DESC" {
				sort.Slice(items, func(i, j int) bool { return integer(items[i]["number"]) > integer(items[j]["number"]) })
			}
			start := integer(f.args["after"])
			if start < 0 || start > len(items) {
				start = 0
			}
			end := len(items)
			if first := integer(f.args["first"]); first > 0 && start+first < end {
				end = start + first
			}
			c := conn(items[start:end])
			c["pageInfo"] = M{"hasNextPage": end < len(items), "hasPreviousPage": start > 0, "endCursor": fmt.Sprint(end), "startCursor": fmt.Sprint(start + 1)}
			out[f.name] = c
		case "issue", "pullRequest":
			list := r.Issues
			if f.name == "pullRequest" {
				list = r.PRs
			}
			for _, v := range list {
				if integer(v["number"]) == integer(f.args["number"]) {
					g := gqlIssue(v)
					if f.name == "pullRequest" {
						g["files"] = conn(r.diff(r.resolve(str(v, "base_ref")), r.resolve(str(v, "head_ref"))))
						g["headRefName"] = v["head_ref"]
						g["baseRefName"] = v["base_ref"]
						g["merged"] = v["merged"]
						g["mergeable"] = "MERGEABLE"
					}
					out[f.name] = g
				}
			}
		}
	}
	return out
}
func (s *Store) graphql(in M) M {
	p := parser{tokens: lex(str(in, "query")), vars: M{}}
	if v, ok := in["variables"].(M); ok {
		p.vars = v
	}
	for p.peek() != "{" && p.peek() != "" {
		p.take()
	}
	fs := p.selection()
	data := M{}
	errs := []M{}
	for _, f := range fs {
		var v any
		switch f.name {
		case "viewer":
			v = M{"login": "adaiversen", "id": "User:1"}
			for _, child := range f.children {
				if child.name != "login" && child.name != "id" && child.name != "name" && child.name != "__typename" {
					errs = append(errs, M{"message": "Unknown field " + child.name})
				}
			}
		case "node":
			s.RLock()
			for _, r := range s.Repos {
				if str(f.args, "id") == fmt.Sprintf("Repository:%d", r.ID) {
					r.Lock()
					v = s.gqlRepo(r, f.children)
					r.Unlock()
				}
			}
			s.RUnlock()
		case "repository":
			func() {
				defer func() { recover() }()
				r := s.repo(str(f.args, "owner"), str(f.args, "name"))
				r.Lock()
				defer r.Unlock()
				v = s.gqlRepo(r, f.children)
			}()
		case "__type":
			name := str(f.args, "name")
			fields := map[string]string{"Issue": "id number title state body", "PullRequest": "id number headRefName baseRefName mergeable state merged files", "Repository": "id databaseId name nameWithOwner owner defaultBranchRef viewerPermission hasIssuesEnabled issue issues pullRequests pullRequest"}
			items := []M{}
			for _, n := range strings.Fields(fields[name]) {
				items = append(items, M{"name": n, "type": M{"name": "String", "kind": "SCALAR"}})
			}
			v = M{"name": name, "fields": items, "enumValues": []M{{"name": "ISSUE"}, {"name": "REPOSITORY"}}}
		case "search":
			items := []M{}
			s.RLock()
			for _, r := range s.Repos {
				r.RLock()
				for _, issue := range r.Issues {
					items = append(items, gqlIssue(issue))
				}
				r.RUnlock()
			}
			s.RUnlock()
			v = conn(items)
			v.(M)["issueCount"] = len(items)
		case "closeIssue", "addComment", "mergePullRequest":
			input, _ := f.args["input"].(M)
			s.RLock()
			for _, r := range s.Repos {
				r.Lock()
				all := append(append([]M{}, r.Issues...), r.PRs...)
				for _, item := range all {
					id := str(item, "node_id")
					if id != str(input, "issueId") && id != str(input, "subjectId") && id != str(input, "pullRequestId") {
						continue
					}
					switch f.name {
					case "closeIssue":
						item["state"] = "closed"
						r.touch()
						v = M{"issue": gqlIssue(item)}
					case "addComment":
						comments := item["comments_list"].([]M)
						comment := M{"id": fmt.Sprintf("Comment:%v:%d", item["id"], len(comments)+1), "body": str(input, "body")}
						item["comments_list"] = append(comments, comment)
						item["comments"] = len(comments) + 1
						r.touch()
						v = M{"commentEdge": M{"node": comment}}
					case "mergePullRequest":
						r.mergePull(item, M{"merge_method": strings.ToLower(str(input, "mergeMethod"))})
						v = M{"pullRequest": gqlIssue(item)}
					}
				}
				r.Unlock()
			}
			s.RUnlock()
		case "createIssue", "createPullRequest", "addPullRequestReview":
			input, _ := f.args["input"].(M)
			if input == nil {
				input = M{}
			}
			id := str(input, "repositoryId")
			s.RLock()
			var repo *Repo
			for _, r := range s.Repos {
				if id == fmt.Sprintf("Repository:%d", r.ID) || id == fmt.Sprint(r.ID) {
					repo = r
				}
			}
			s.RUnlock()
			if repo == nil {
				errs = append(errs, M{"message": "Unknown repository id"})
				break
			}
			repo.Lock()
			func() {
				defer repo.Unlock()
				if f.name == "createIssue" {
					v = M{"issue": gqlIssue(repo.newIssue(input))}
				} else if f.name == "createPullRequest" {
					input["head"] = input["headRefName"]
					input["base"] = input["baseRefName"]
					pull := repo.newPull(input)
					g := gqlIssue(pull)
					g["headRefName"] = pull["head_ref"]
					g["baseRefName"] = pull["base_ref"]
					v = M{"pullRequest": g}
				}
			}()
		default:
			errs = append(errs, M{"message": "Unknown field " + f.name})
		}
		data[f.alias] = project(v, f.children)
	}
	out := M{"data": data}
	if len(errs) > 0 {
		out["errors"] = errs
	}
	return out
}
