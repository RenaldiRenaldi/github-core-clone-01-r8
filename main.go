package main

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
)

type object struct {
	kind string
	data []byte
}
type forge struct {
	mu      sync.RWMutex
	objects map[string]object
	refs    map[string]string
	cache   map[string][]byte
}

func oid(o object) string {
	b := []byte(fmt.Sprintf("%s %d\x00", o.kind, len(o.data)))
	b = append(b, o.data...)
	h := sha1.Sum(b)
	return hex.EncodeToString(h[:])
}
func (f *forge) put(o object) string { id := oid(o); f.objects[id] = o; return id }
func (f *forge) commit(tree, parent, msg string) string {
	s := fmt.Sprintf("tree %s\n", tree)
	if parent != "" {
		s += fmt.Sprintf("parent %s\n", parent)
	}
	s += "author Forge <forge@example.test> 1700000000 +0000\ncommitter Forge <forge@example.test> 1700000000 +0000\n\n" + msg + "\n"
	return f.put(object{"commit", []byte(s)})
}
func decode(s string) []byte { b, _ := hex.DecodeString(s); return b }
func seed() *forge {
	f := &forge{objects: map[string]object{}, refs: map[string]string{}, cache: map[string][]byte{}}
	blob := f.put(object{"blob", []byte("Hello from the in-process Go forge.\n")})
	ent := append([]byte("100644 README.md\x00"), decode(blob)...)
	tree := f.put(object{"tree", ent})
	first := f.commit(tree, "", "Initial demo")
	second := f.commit(tree, first, "Add greeting")
	f.refs["refs/heads/main"] = second
	return f
}
func pkt(s string) []byte { return []byte(fmt.Sprintf("%04x%s", len(s)+4, s)) }
func pack(f *forge, ids []string) []byte {
	var b bytes.Buffer
	b.WriteString("PACK")
	b.Write([]byte{0, 0, 0, 2, byte(len(ids) >> 24), byte(len(ids) >> 16), byte(len(ids) >> 8), byte(len(ids))})
	for _, id := range ids {
		o := f.objects[id]
		typ := map[string]byte{"commit": 1, "tree": 2, "blob": 3, "tag": 4}[o.kind]
		n := len(o.data)
		c := typ<<4 | byte(n&15)
		n >>= 4
		for n > 0 {
			b.WriteByte(c | 128)
			c = byte(n & 127)
			n >>= 7
		}
		b.WriteByte(c)
		z := zlib.NewWriter(&b)
		z.Write(o.data)
		z.Close()
	}
	h := sha1.Sum(b.Bytes())
	b.Write(h[:])
	return b.Bytes()
}
func (f *forge) closure(root string, depth int) ([]string, []string) {
	seen := map[string]bool{}
	boundary := []string{}
	type item struct {
		id string
		d  int
	}
	q := []item{{root, 1}}
	for len(q) > 0 {
		x := q[0]
		q = q[1:]
		if seen[x.id] {
			continue
		}
		o, ok := f.objects[x.id]
		if !ok {
			continue
		}
		seen[x.id] = true
		if o.kind == "commit" {
			var tree string
			parents := []string{}
			for _, l := range strings.Split(string(o.data), "\n") {
				if strings.HasPrefix(l, "tree ") {
					tree = strings.TrimPrefix(l, "tree ")
				} else if strings.HasPrefix(l, "parent ") {
					parents = append(parents, strings.TrimPrefix(l, "parent "))
				}
			}
			if tree != "" {
				q = append(q, item{tree, x.d})
			}
			if x.d >= depth && len(parents) > 0 {
				boundary = append(boundary, x.id)
			} else {
				for _, p := range parents {
					q = append(q, item{p, x.d + 1})
				}
			}
		} else if o.kind == "tree" {
			for i := 0; i < len(o.data); {
				z := bytes.IndexByte(o.data[i:], 0)
				if z < 0 || i+z+21 > len(o.data) {
					break
				}
				off := i + z + 1
				id := hex.EncodeToString(o.data[off : off+20])
				q = append(q, item{id, x.d})
				i = off + 20
			}
		}
	}
	ids := make([]string, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids, boundary
}
func (f *forge) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/healthz" {
		w.Header().Set("content-type", "application/json")
		json.NewEncoder(w).Encode(map[string]bool{"ok": true})
		return
	}
	p := strings.Trim(r.URL.Path, "/")
	if !strings.HasSuffix(p, ".git/info/refs") && !strings.HasSuffix(p, ".git/git-upload-pack") && !strings.HasSuffix(p, ".git/git-receive-pack") {
		http.NotFound(w, r)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if strings.HasSuffix(p, "info/refs") {
		svc := r.URL.Query().Get("service")
		w.Header().Set("content-type", "application/x-"+svc+"-advertisement")
		if strings.Contains(r.Header.Get("Git-Protocol"), "version=2") && svc == "git-upload-pack" {
			for _, s := range []string{"version 2\n", "agent=forge-r8\n", "ls-refs=unborn\n", "fetch=shallow filter\n", "object-format=sha1\n"} {
				w.Write(pkt(s))
			}
			w.Write([]byte("0000"))
			return
		}
		w.Write(pkt("# service=" + svc + "\n"))
		w.Write([]byte("0000"))
		id := f.refs["refs/heads/main"]
		caps := "multi_ack_detailed side-band-64k thin-pack ofs-delta shallow filter symref=HEAD:refs/heads/main"
		w.Write(pkt(id + " HEAD\x00" + caps + "\n"))
		w.Write(pkt(id + " refs/heads/main\n"))
		w.Write([]byte("0000"))
		return
	}
	if strings.HasSuffix(p, "git-upload-pack") {
		f.upload(w, r)
		return
	}
	http.Error(w, "receive-pack unavailable", 501)
}
func (f *forge) upload(w http.ResponseWriter, r *http.Request) {
	raw := new(bytes.Buffer)
	raw.ReadFrom(r.Body)
	key := fmt.Sprintf("%x", sha1.Sum(raw.Bytes()))
	if v := f.cache[key]; v != nil {
		w.Header().Set("content-type", "application/x-git-upload-pack-result")
		w.Write(v)
		return
	}
	v2 := strings.Contains(r.Header.Get("Git-Protocol"), "version=2")
	lines := []string{}
	data := raw.Bytes()
	for i := 0; i+4 <= len(data); {
		n, e := hex.DecodeString(string(data[i : i+4]))
		if e != nil || len(n) == 0 {
			break
		}
		size := int(n[0])*256 + int(n[1])
		i += 4
		if size == 0 || size == 1 {
			continue
		}
		if i+size-4 > len(data) {
			break
		}
		lines = append(lines, string(data[i:i+size-4]))
		i += size - 4
	}
	var out bytes.Buffer
	wants := []string{}
	depth := 999999
	for _, l := range lines {
		if strings.HasPrefix(l, "want ") {
			wants = append(wants, strings.Fields(strings.TrimPrefix(l, "want "))[0])
		}
		if strings.HasPrefix(l, "deepen ") {
			fmt.Sscanf(l, "deepen %d", &depth)
		}
	}
	if v2 && strings.Contains(string(data), "command=ls-refs") {
		out.Write(pkt(f.refs["refs/heads/main"] + " HEAD symref-target:refs/heads/main\n"))
		out.Write(pkt(f.refs["refs/heads/main"] + " refs/heads/main\n"))
		out.WriteString("0000")
		w.Header().Set("content-type", "application/x-git-upload-pack-result")
		w.Write(out.Bytes())
		return
	}
	if v2 && len(wants) == 0 {
		out.WriteString("0000")
	} else if len(wants) > 0 {
		ids, bounds := f.closure(wants[0], depth)
		p := pack(f, ids)
		if v2 {
			if len(bounds) > 0 {
				out.Write(pkt("shallow-info\n"))
				for _, id := range bounds {
					out.Write(pkt("shallow " + id + "\n"))
				}
				out.WriteString("0001")
			}
			out.Write(pkt("packfile\n"))
			for len(p) > 0 {
				n := len(p)
				if n > 65515 {
					n = 65515
				}
				chunk := append([]byte{1}, p[:n]...)
				out.Write(pkt(string(chunk)))
				p = p[n:]
			}
			out.WriteString("0000")
		} else {
			if len(bounds) > 0 {
				for _, id := range bounds {
					out.Write(pkt("shallow " + id + "\n"))
				}
				out.WriteString("0000")
			}
			out.Write(pkt("NAK\n"))
			out.Write(pkt(string(append([]byte{1}, p...))))
			out.WriteString("0000")
		}
	}
	b := out.Bytes()
	f.cache[key] = append([]byte(nil), b...)
	w.Header().Set("content-type", "application/x-git-upload-pack-result")
	w.Write(b)
}
func main() {
	f := seed()
	p := os.Getenv("PORT")
	if p == "" {
		p = "8080"
	}
	http.ListenAndServe(":"+p, f)
}
