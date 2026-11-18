package main

import (
	"bufio"
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
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
		if svc == "git-receive-pack" {
			caps = "report-status delete-refs ofs-delta"
		}
		w.Write(pkt(id + " HEAD\x00" + caps + "\n"))
		w.Write(pkt(id + " refs/heads/main\n"))
		w.Write([]byte("0000"))
		return
	}
	if strings.HasSuffix(p, "git-upload-pack") {
		f.upload(w, r)
		return
	}
	if strings.HasSuffix(p, "git-receive-pack") {
		w.Header().Set("content-type", "application/x-git-receive-pack-result")
		w.Write(f.receive(r))
		return
	}
}

func readVarint(b []byte, i *int) int {
	n, shift := 0, uint(0)
	for {
		if *i >= len(b) || shift > 56 {
			panic("bad delta")
		}
		c := b[*i]
		*i++
		n |= int(c&127) << shift
		if c&128 == 0 {
			return n
		}
		shift += 7
	}
}
func applyDelta(base, d []byte) []byte {
	i := 0
	if readVarint(d, &i) != len(base) {
		panic("delta base size")
	}
	size := readVarint(d, &i)
	if size > 64<<20 {
		panic("delta size")
	}
	out := make([]byte, 0, size)
	for i < len(d) {
		c := d[i]
		i++
		if c&128 != 0 {
			off, n := 0, 0
			for j := 0; j < 4; j++ {
				if c&(1<<j) != 0 {
					if i >= len(d) {
						panic("delta offset")
					}
					off |= int(d[i]) << (8 * j)
					i++
				}
			}
			for j := 0; j < 3; j++ {
				if c&(1<<(4+j)) != 0 {
					if i >= len(d) {
						panic("delta length")
					}
					n |= int(d[i]) << (8 * j)
					i++
				}
			}
			if n == 0 {
				n = 65536
			}
			if off+n > len(base) {
				panic("delta bounds")
			}
			out = append(out, base[off:off+n]...)
		} else {
			n := int(c)
			if n == 0 || i+n > len(d) {
				panic("delta insert")
			}
			out = append(out, d[i:i+n]...)
			i += n
		}
		if len(out) > size {
			panic("delta overflow")
		}
	}
	if len(out) != size {
		panic("delta size mismatch")
	}
	return out
}
func inflatePack(raw []byte, existing map[string]object) map[string]object {
	if len(raw) < 32 || string(raw[:4]) != "PACK" {
		panic("missing pack")
	}
	version := binary.BigEndian.Uint32(raw[4:8])
	if version != 2 && version != 3 {
		panic("pack version")
	}
	count := binary.BigEndian.Uint32(raw[8:12])
	if count > 100000 {
		panic("pack count")
	}
	type item struct {
		off, typ, base int
		baseID         string
		data           []byte
	}
	items := []item{}
	pos := 12
	for k := uint32(0); k < count; k++ {
		off := pos
		if pos >= len(raw)-20 {
			panic("pack eof")
		}
		c := raw[pos]
		pos++
		typ := int(c >> 4 & 7)
		size := int(c & 15)
		shift := uint(4)
		for c&128 != 0 {
			if pos >= len(raw)-20 || shift > 56 {
				panic("pack size")
			}
			c = raw[pos]
			pos++
			size |= int(c&127) << shift
			shift += 7
		}
		it := item{off: off, typ: typ}
		if typ == 6 {
			c = raw[pos]
			pos++
			dist := int(c & 127)
			for c&128 != 0 {
				c = raw[pos]
				pos++
				dist = ((dist + 1) << 7) | int(c&127)
			}
			it.base = off - dist
		} else if typ == 7 {
			if pos+20 > len(raw)-20 {
				panic("ref delta")
			}
			it.baseID = hex.EncodeToString(raw[pos : pos+20])
			pos += 20
		} else if map[int]string{1: "commit", 2: "tree", 3: "blob", 4: "tag"}[typ] == "" {
			panic("object type")
		}
		z, e := zlib.NewReader(bytes.NewReader(raw[pos:]))
		if e != nil {
			panic("zlib")
		}
		data, e := io.ReadAll(io.LimitReader(z, 64<<20+1))
		z.Close()
		if e != nil || len(data) != size {
			panic("object size")
		}
		pos += zlibLength(raw[pos:])
		it.data = data
		items = append(items, it)
	}
	sum := sha1.Sum(raw[:pos])
	if pos+20 != len(raw) || !bytes.Equal(raw[pos:], sum[:]) {
		panic("pack checksum")
	}
	out := map[string]object{}
	byOff := map[int]object{}
	remaining := items
	for len(remaining) > 0 {
		next := []item{}
		for _, it := range remaining {
			var o object
			if it.typ == 6 || it.typ == 7 {
				var base object
				var ok bool
				if it.typ == 6 {
					base, ok = byOff[it.base]
				} else {
					base, ok = out[it.baseID]
					if !ok {
						base, ok = existing[it.baseID]
					}
				}
				if !ok {
					next = append(next, it)
					continue
				}
				o = object{base.kind, applyDelta(base.data, it.data)}
			} else {
				o = object{map[int]string{1: "commit", 2: "tree", 3: "blob", 4: "tag"}[it.typ], it.data}
			}
			id := oid(o)
			out[id] = o
			byOff[it.off] = o
		}
		if len(next) == len(remaining) {
			panic("unresolved delta")
		}
		remaining = next
	}
	return out
}
func zlibLength(raw []byte) int {
	r := bytes.NewReader(raw)
	br := bufio.NewReader(r)
	z, e := zlib.NewReader(br)
	if e != nil {
		panic("zlib")
	}
	if _, e = io.Copy(io.Discard, z); e != nil {
		panic("zlib data")
	}
	z.Close()
	return len(raw) - r.Len() - br.Buffered()
}
func (f *forge) receive(r *http.Request) []byte {
	defer func() {}()
	data, _ := io.ReadAll(io.LimitReader(r.Body, 64<<20))
	lines, at := []string{}, 0
	for at+4 <= len(data) {
		n, e := strconv.ParseUint(string(data[at:at+4]), 16, 16)
		if e != nil {
			break
		}
		at += 4
		if n == 0 {
			break
		}
		if n < 4 || at+int(n)-4 > len(data) {
			break
		}
		lines = append(lines, string(data[at:at+int(n)-4]))
		at += int(n) - 4
	}
	if len(lines) == 0 {
		return []byte("0000")
	}
	staged := map[string]object{}
	unpack := "ok"
	if at < len(data) {
		func() {
			defer func() {
				if recover() != nil {
					unpack = "invalid pack"
				}
			}()
			staged = inflatePack(data[at:], f.objects)
		}()
	}
	type command struct{ old, next, ref, err string }
	cmds := []command{}
	for _, l := range lines {
		fields := strings.Fields(strings.SplitN(l, "\x00", 2)[0])
		if len(fields) != 3 {
			continue
		}
		cmds = append(cmds, command{fields[0], fields[1], fields[2], ""})
	}
	objects := make(map[string]object, len(f.objects)+len(staged))
	for k, v := range f.objects {
		objects[k] = v
	}
	for k, v := range staged {
		objects[k] = v
	}
	for i := range cmds {
		c := &cmds[i]
		cur := f.refs[c.ref]
		if cur == "" {
			cur = strings.Repeat("0", 40)
		}
		switch {
		case unpack != "ok":
			c.err = "unpacker error"
		case cur != c.old:
			c.err = "stale old object id"
		case c.next != strings.Repeat("0", 40):
			if _, ok := objects[c.next]; !ok {
				c.err = "missing object"
			} else if strings.HasPrefix(c.ref, "refs/heads/") && objects[c.next].kind != "commit" {
				c.err = "branch target must be commit"
			}
		}
	}
	for _, c := range cmds {
		if c.err == "" {
			if c.next == strings.Repeat("0", 40) {
				delete(f.refs, c.ref)
			} else {
				f.refs[c.ref] = c.next
			}
		}
	}
	if unpack == "ok" {
		for k, v := range staged {
			f.objects[k] = v
		}
	}
	var out bytes.Buffer
	out.Write(pkt("unpack " + unpack + "\n"))
	for _, c := range cmds {
		if c.err != "" {
			out.Write(pkt("ng " + c.ref + " " + c.err + "\n"))
		} else {
			out.Write(pkt("ok " + c.ref + "\n"))
		}
	}
	out.WriteString("0000")
	if strings.Contains(strings.SplitN(lines[0], "\x00", 2)[1], "side-band-64k") {
		var band bytes.Buffer
		for payload := out.Bytes(); len(payload) > 0; {
			n := len(payload)
			if n > 65500 {
				n = 65500
			}
			band.Write(pkt(string(append([]byte{1}, payload[:n]...))))
			payload = payload[n:]
		}
		band.WriteString("0000")
		return band.Bytes()
	}
	return out.Bytes()
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
