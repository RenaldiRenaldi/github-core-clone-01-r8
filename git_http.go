package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

func gitHTTP(w http.ResponseWriter, q *http.Request, r *Repo, p []string) {
	service := p[0]
	if service == "info" {
		service = q.URL.Query().Get("service")
	}
	if service != "git-upload-pack" && service != "git-receive-pack" {
		fail(400, "Unknown git service")
	}
	v2 := strings.Contains(q.Header.Get("Git-Protocol"), "version=2") && service == "git-upload-pack"
	if p[0] == "info" {
		w.Header().Set("content-type", "application/x-"+service+"-advertisement")
		w.Write(advertise(r, service, v2))
		return
	}
	if q.Method != "POST" {
		fail(405, "POST required")
	}
	if service == "git-receive-pack" {
		w.Header().Set("content-type", "application/x-git-receive-pack-result")
		w.Write(receiveReader(r, http.MaxBytesReader(w, q.Body, 64<<20)))
		return
	}
	raw, e := io.ReadAll(io.LimitReader(q.Body, 64<<20))
	if e != nil {
		fail(400, "Read error")
	}
	w.Header().Set("content-type", "application/x-"+service+"-result")
	if service == "git-receive-pack" {
		w.Write(receive(r, raw))
		return
	}
	key := fmt.Sprintf("%v:%x", v2, sha256.Sum256(raw))
	if b := r.Cache[key]; b != nil {
		w.Write(b)
		return
	}
	b := upload(r, raw, v2)
	if len(r.Cache) >= 32 {
		r.Cache = map[string][]byte{}
	}
	if len(b) < 8<<20 {
		r.Cache[key] = b
	}
	w.Write(b)
}
func refNames(r *Repo) []string {
	keys := []string{}
	for k := range r.Refs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
func advertise(r *Repo, service string, v2 bool) []byte {
	var b bytes.Buffer
	if v2 {
		for _, s := range []string{"version 2\n", "agent=go-forge/4\n", "ls-refs=unborn\n", "fetch=shallow filter\n", "object-format=sha1\n"} {
			b.Write(pkt(s))
		}
		b.WriteString("0000")
		return b.Bytes()
	}
	b.Write(pkt("# service=" + service + "\n"))
	b.WriteString("0000")
	caps := "report-status delete-refs side-band-64k ofs-delta atomic"
	if service == "git-upload-pack" {
		caps = "multi_ack_detailed side-band-64k thin-pack ofs-delta shallow filter symref=HEAD:refs/heads/main"
	}
	keys := refNames(r)
	if r.Refs["refs/heads/main"] != "" {
		keys = append([]string{"HEAD"}, keys...)
	}
	if len(keys) == 0 {
		b.Write(pkt(zero + " capabilities^{}\x00" + caps + "\n"))
	}
	for i, k := range keys {
		id := r.Refs[k]
		if k == "HEAD" {
			id = r.Refs["refs/heads/main"]
		}
		s := id + " " + k
		if i == 0 {
			s += "\x00" + caps
		}
		b.Write(pkt(s + "\n"))
	}
	b.WriteString("0000")
	return b.Bytes()
}
func upload(r *Repo, raw []byte, v2 bool) []byte {
	lines := []string{}
	for i := 0; i+4 <= len(raw); {
		var n int
		fmt.Sscanf(string(raw[i:i+4]), "%04x", &n)
		i += 4
		if n < 4 {
			continue
		}
		if i+n-4 > len(raw) {
			break
		}
		lines = append(lines, strings.TrimSpace(string(raw[i:i+n-4])))
		i += n - 4
	}
	get := func(prefix string) []string {
		out := []string{}
		for _, l := range lines {
			if strings.HasPrefix(l, prefix+" ") {
				out = append(out, strings.TrimPrefix(l, prefix+" "))
			}
		}
		return out
	}
	has := func(s string) bool {
		for _, l := range lines {
			if l == s {
				return true
			}
		}
		return false
	}
	var b bytes.Buffer
	if has("command=ls-refs") {
		prefixes := get("ref-prefix")
		match := func(s string) bool {
			if len(prefixes) == 0 {
				return true
			}
			for _, p := range prefixes {
				if strings.HasPrefix(s, p) {
					return true
				}
			}
			return false
		}
		if id := r.Refs["refs/heads/main"]; id != "" && match("HEAD") {
			extra := ""
			if has("symrefs") {
				extra = " symref-target:refs/heads/main"
			}
			b.Write(pkt(id + " HEAD" + extra + "\n"))
		}
		for _, ref := range refNames(r) {
			if match(ref) {
				extra := ""
				id := r.Refs[ref]
				if has("peel") && r.Objects[id].Type == "tag" {
					target := id
					for r.Objects[target].Type == "tag" {
						target = links(r.Objects[target])[0]
					}
					extra = " peeled:" + target
				}
				b.Write(pkt(id + " " + ref + extra + "\n"))
			}
		}
		b.WriteString("0000")
		return b.Bytes()
	}
	wants := get("want")
	if len(wants) == 0 {
		return []byte("0000")
	}
	roots := []string{}
	for _, w := range wants {
		roots = append(roots, strings.Fields(w)[0])
	}
	depth := int(^uint(0) >> 1)
	if d := get("deepen"); len(d) > 0 {
		depth, _ = strconv.Atoi(d[0])
		if depth < 1 {
			fail(400, "Invalid depth")
		}
	}
	selected := map[string]bool{}
	boundary := []string{}
	type node struct {
		id string
		d  int
	}
	queue := []node{}
	for _, id := range roots {
		queue = append(queue, node{id, 1})
	}
	for len(queue) > 0 {
		n := queue[0]
		queue = queue[1:]
		if selected[n.id] {
			continue
		}
		o, ok := r.Objects[n.id]
		if !ok {
			fail(400, "Unknown want")
		}
		selected[n.id] = true
		if o.Type == "commit" {
			tree, parents := info(o)
			for id := range closure(r.Objects, []string{tree}) {
				selected[id] = true
			}
			if n.d >= depth && len(parents) > 0 {
				boundary = append(boundary, n.id)
			} else {
				for _, id := range parents {
					queue = append(queue, node{id, n.d + 1})
				}
			}
		} else {
			for _, id := range links(o) {
				queue = append(queue, node{id, n.d})
			}
		}
	}
	for _, h := range get("have") {
		if _, ok := r.Objects[h]; ok {
			if len(get("shallow")) == 0 {
				for id := range closure(r.Objects, []string{h}) {
					delete(selected, id)
				}
			} else {
				delete(selected, h)
			}
		}
	}
	filter := ""
	if f := get("filter"); len(f) > 0 {
		filter = f[0]
	}
	ids := []string{}
	explicit := map[string]bool{}
	for _, id := range roots {
		explicit[id] = true
	}
	for id := range selected {
		o := r.Objects[id]
		if o.Type == "blob" && !explicit[id] {
			if filter == "blob:none" {
				continue
			}
			if strings.HasPrefix(filter, "blob:limit=") {
				n, _ := strconv.Atoi(strings.TrimPrefix(filter, "blob:limit="))
				if len(o.Data) >= n {
					continue
				}
			}
		}
		ids = append(ids, id)
	}
	sort.Strings(ids)
	data := pack(r.Objects, ids)
	if v2 {
		if !has("done") {
			b.Write(pkt("acknowledgments\n"))
			known := false
			for _, h := range get("have") {
				if _, ok := r.Objects[h]; ok {
					b.Write(pkt("ACK " + h + "\n"))
					known = true
				}
			}
			if !known {
				b.Write(pkt("NAK\n"))
			}
			b.Write(pkt("ready\n"))
			b.WriteString("0001")
		}
		if len(boundary) > 0 || len(get("deepen")) > 0 {
			b.Write(pkt("shallow-info\n"))
			for _, id := range boundary {
				b.Write(pkt("shallow " + id + "\n"))
			}
			for _, id := range get("shallow") {
				if selected[id] {
					isBoundary := false
					for _, v := range boundary {
						if v == id {
							isBoundary = true
						}
					}
					if !isBoundary {
						b.Write(pkt("unshallow " + id + "\n"))
					}
				}
			}
			b.WriteString("0001")
		}
		b.Write(pkt("packfile\n"))
		b.Write(sideband(data))
		b.WriteString("0000")
	} else {
		if len(boundary) > 0 || len(get("deepen")) > 0 {
			for _, id := range boundary {
				b.Write(pkt("shallow " + id + "\n"))
			}
			b.WriteString("0000")
		}
		b.Write(pkt("NAK\n"))
		if has("done") {
			if strings.Contains(strings.Join(lines, " "), "side-band") {
				b.Write(sideband(data))
				b.WriteString("0000")
			} else {
				b.Write(data)
			}
		}
	}
	return b.Bytes()
}
func receive(r *Repo, raw []byte) []byte { return receiveReader(r, bytes.NewReader(raw)) }
func receiveReader(r *Repo, input io.Reader) []byte {
	reader := bufio.NewReader(input)
	lines := []string{}
	for {
		header := make([]byte, 4)
		if _, e := io.ReadFull(reader, header); e != nil {
			fail(400, "Ref command EOF")
		}
		n, e := strconv.ParseUint(string(header), 16, 16)
		if e != nil {
			fail(400, "Bad pkt length")
		}
		if n == 0 {
			break
		}
		if n < 4 {
			fail(400, "Bad pkt length")
		}
		line := make([]byte, int(n)-4)
		if _, e := io.ReadFull(reader, line); e != nil {
			fail(400, "Truncated command")
		}
		lines = append(lines, string(line))
		if len(lines) > 10000 {
			fail(400, "Too many refs")
		}
	}
	if len(lines) == 0 {
		return []byte("0000")
	}
	caps := ""
	if p := strings.SplitN(lines[0], "\x00", 2); len(p) == 2 {
		caps = p[1]
	}
	staged := map[string]Object{}
	unpack := "ok"
	func() {
		defer func() {
			if recover() != nil {
				unpack = "invalid pack"
			}
		}()
		if _, e := reader.Peek(1); e != io.EOF {
			staged = parsePackReader(reader, r.Objects)
		}
	}()
	ob := map[string]Object{}
	for k, v := range r.Objects {
		ob[k] = v
	}
	for k, v := range staged {
		ob[k] = v
	}
	type cmd struct{ old, next, ref, err string }
	commands := []cmd{}
	counts := map[string]int{}
	for _, l := range lines {
		f := strings.Fields(strings.SplitN(l, "\x00", 2)[0])
		if len(f) != 3 {
			fail(400, "Bad ref command")
		}
		commands = append(commands, cmd{old: f[0], next: f[1], ref: f[2]})
		counts[f[2]]++
	}
	bad := false
	for i := range commands {
		c := &commands[i]
		current := r.Refs[c.ref]
		if current == "" {
			current = zero
		}
		switch {
		case unpack != "ok":
			c.err = "unpacker error"
		case !isSHA(c.old) || !isSHA(c.next):
			c.err = "invalid object id"
		case c.old == zero && c.next == zero:
			c.err = "cannot delete absent ref"
		case !validRef(c.ref):
			c.err = "invalid ref name"
		case counts[c.ref] > 1:
			c.err = "duplicate ref"
		case current != c.old:
			c.err = "stale old object id"
		case r.Protection[strings.TrimPrefix(c.ref, "refs/heads/")] != nil:
			c.err = "protected branch"
		case c.next != zero:
			func() {
				defer func() {
					if recover() != nil {
						c.err = "missing object"
					}
				}()
				closure(ob, []string{c.next})
			}()
			if c.err == "" && strings.HasPrefix(c.ref, "refs/heads/") {
				if ob[c.next].Type != "commit" {
					c.err = "branch target must be commit"
				} else if c.old != zero && !ancestor(ob, c.old, c.next) {
					c.err = "non-fast-forward"
				}
			}
		}
		if c.err != "" {
			bad = true
		}
	}
	// Ref namespaces cannot contain both a parent and a descendant.
	proposed := refNames(r)
	for _, c := range commands {
		if c.next != zero {
			proposed = append(proposed, c.ref)
		}
	}
	for i := range commands {
		c := &commands[i]
		if c.next == zero {
			continue
		}
		for _, other := range proposed {
			if other != c.ref && (strings.HasPrefix(other, c.ref+"/") || strings.HasPrefix(c.ref, other+"/")) {
				c.err = "ref namespace collision"
				bad = true
			}
		}
	}
	accepted := false
	for i := range commands {
		c := &commands[i]
		if bad && strings.Contains(caps, "atomic") && c.err == "" {
			c.err = "atomic transaction aborted"
		}
		if c.err == "" {
			accepted = true
			if c.next == zero {
				delete(r.Refs, c.ref)
			} else {
				r.Refs[c.ref] = c.next
			}
		}
	}
	if accepted {
		for k, v := range staged {
			r.Objects[k] = v
		}
		r.touch()
	}
	var report bytes.Buffer
	report.Write(pkt("unpack " + unpack + "\n"))
	for _, c := range commands {
		line := "ok " + c.ref + "\n"
		if c.err != "" {
			line = "ng " + c.ref + " " + c.err + "\n"
		}
		report.Write(pkt(line))
	}
	report.WriteString("0000")
	if strings.Contains(caps, "side-band-64k") {
		return append(sideband(report.Bytes()), []byte("0000")...)
	}
	return report.Bytes()
}
