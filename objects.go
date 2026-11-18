package main

import (
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Object bytes are immutable after publication; graph walks never modify them.
type Object struct {
	Type string
	Data []byte
}

const zero = "0000000000000000000000000000000000000000"

func oid(o Object) string {
	h := sha1.New()
	fmt.Fprintf(h, "%s %d\x00", o.Type, len(o.Data))
	h.Write(o.Data)
	return hex.EncodeToString(h.Sum(nil))
}
func (r *Repo) put(t string, d []byte) string {
	o := Object{t, d}
	id := oid(o)
	r.Objects[id] = o
	return id
}

type Entry struct{ Mode, SHA string }

func info(o Object) (string, []string) {
	var tree string
	var parents []string
	for _, l := range strings.Split(string(o.Data), "\n") {
		if l == "" {
			break
		}
		if strings.HasPrefix(l, "tree ") {
			tree = l[5:]
		}
		if strings.HasPrefix(l, "parent ") {
			parents = append(parents, l[7:])
		}
	}
	return tree, parents
}
func links(o Object) []string {
	switch o.Type {
	case "commit":
		t, p := info(o)
		return append([]string{t}, p...)
	case "tag":
		return []string{strings.Fields(string(o.Data))[1]}
	case "tree":
		var ids []string
		b := o.Data
		for len(b) > 0 {
			i := bytes.IndexByte(b, 0)
			if i < 0 || len(b) < i+21 {
				fail(400, "Invalid tree")
			}
			if !bytes.HasPrefix(b, []byte("160000 ")) {
				ids = append(ids, hex.EncodeToString(b[i+1:i+21]))
			}
			b = b[i+21:]
		}
		return ids
	}
	return nil
}
func closure(ob map[string]Object, roots []string) map[string]bool {
	seen := map[string]bool{}
	q := append([]string{}, roots...)
	for len(q) > 0 {
		id := q[len(q)-1]
		q = q[:len(q)-1]
		if seen[id] {
			continue
		}
		o, ok := ob[id]
		if !ok {
			fail(400, "Missing object "+id)
		}
		seen[id] = true
		q = append(q, links(o)...)
	}
	return seen
}
func ancestor(ob map[string]Object, a, b string) bool {
	q := []string{b}
	seen := map[string]bool{}
	for len(q) > 0 {
		id := q[0]
		q = q[1:]
		if id == a {
			return true
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		_, p := info(ob[id])
		q = append(q, p...)
	}
	return false
}
func (r *Repo) resolve(ref string) string {
	if id := r.Refs["refs/heads/"+ref]; id != "" {
		return id
	}
	if id := r.Refs[ref]; id != "" {
		return id
	}
	if ref == "" || ref == "HEAD" || ref == "main" && len(r.Refs) == 0 {
		return r.Refs["refs/heads/main"]
	}
	if _, ok := r.Objects[ref]; ok {
		return ref
	}
	fail(404, "Unknown ref")
	return ""
}
func (r *Repo) files(ref string) map[string]Entry {
	out := map[string]Entry{}
	id := r.resolve(ref)
	if id == "" {
		return out
	}
	o := r.Objects[id]
	if o.Type == "commit" {
		id, _ = info(o)
	}
	var walk func(string, string)
	walk = func(id, prefix string) {
		b := r.Objects[id].Data
		for len(b) > 0 {
			i := bytes.IndexByte(b, 0)
			if i < 0 || len(b) < i+21 {
				fail(400, "Bad tree")
			}
			p := strings.SplitN(string(b[:i]), " ", 2)
			sha := hex.EncodeToString(b[i+1 : i+21])
			if p[0] == "40000" || p[0] == "040000" {
				walk(sha, prefix+p[1]+"/")
			} else {
				out[prefix+p[1]] = Entry{p[0], sha}
			}
			b = b[i+21:]
		}
	}
	walk(id, "")
	return out
}
func (r *Repo) tree(files map[string]Entry) string {
	var build func(string) string
	build = func(prefix string) string {
		items := map[string]Entry{}
		for path, e := range files {
			if !strings.HasPrefix(path, prefix) {
				continue
			}
			rest := strings.TrimPrefix(path, prefix)
			if i := strings.IndexByte(rest, '/'); i >= 0 {
				dir := rest[:i]
				if _, ok := items[dir]; !ok {
					items[dir] = Entry{"40000", build(prefix + dir + "/")}
				}
			} else {
				items[rest] = e
			}
		}
		keys := []string{}
		for k := range items {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(i, j int) bool {
			a, b := keys[i], keys[j]
			if items[a].Mode == "40000" {
				a += "/"
			}
			if items[b].Mode == "40000" {
				b += "/"
			}
			return a < b
		})
		var b bytes.Buffer
		for _, k := range keys {
			e := items[k]
			fmt.Fprintf(&b, "%s %s\x00", e.Mode, k)
			d, _ := hex.DecodeString(e.SHA)
			b.Write(d)
		}
		return r.put("tree", b.Bytes())
	}
	return build("")
}
func (r *Repo) commit(tree string, parents []string, msg string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "tree %s\n", tree)
	for _, p := range parents {
		if p != "" {
			fmt.Fprintf(&b, "parent %s\n", p)
		}
	}
	b.WriteString("author Forge <forge@example.test> 1794873600 +0000\ncommitter Forge <forge@example.test> 1794873600 +0000\n\n" + msg + "\n")
	return r.put("commit", []byte(b.String()))
}
func validRef(s string) bool {
	if !strings.HasPrefix(s, "refs/") || strings.ContainsAny(s, " ~^:?*[\\\x00\n") || strings.Contains(s, "..") || strings.Contains(s, "@{") || strings.Contains(s, "//") {
		return false
	}
	for _, p := range strings.Split(s, "/") {
		if p == "" || strings.HasPrefix(p, ".") || strings.HasSuffix(p, ".") || strings.HasSuffix(p, ".lock") {
			return false
		}
	}
	return true
}
func integer(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case string:
		x, _ := strconv.Atoi(n)
		return x
	}
	return 0
}

// Commit ancestry is immutable, so it remains valid across ref mutations.
func (r *Repo) history(root string) map[string]bool {
	if r.Graph == nil {
		r.Graph = map[string]map[string]bool{}
	}
	if got := r.Graph[root]; got != nil {
		return got
	}
	seen := map[string]bool{}
	queue := []string{root}
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		if seen[id] {
			continue
		}
		seen[id] = true
		_, parents := info(r.Objects[id])
		queue = append(queue, parents...)
	}
	if len(r.Graph) >= 64 {
		r.Graph = map[string]map[string]bool{}
	}
	r.Graph[root] = seen
	return seen
}

func isSHA(s string) bool {
	if len(s) != 40 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}
