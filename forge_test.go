package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func fixture() *Repo {
	s := NewStore()
	r := s.create("adaiversen", "sample")
	blob := r.put("blob", []byte("base\n"))
	r.Refs["refs/heads/main"] = r.commit(r.tree(map[string]Entry{"base.txt": {"100644", blob}}), nil, "base")
	return r
}
func TestHealth(t *testing.T) {
	rec := httptest.NewRecorder()
	NewStore().ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != 200 {
		t.Fatal(rec.Code)
	}
}
func TestPackRoundTrip(t *testing.T) {
	r := fixture()
	ids := []string{}
	for id := range r.Objects {
		ids = append(ids, id)
	}
	p := pack(r.Objects, ids)
	got := parsePack(p, nil)
	if len(got) != len(ids) {
		t.Fatal(len(got))
	}
	p[len(p)-1] ^= 1
	defer func() {
		if recover() == nil {
			t.Fatal("checksum accepted")
		}
	}()
	parsePack(p, nil)
}
func TestDelta(t *testing.T) {
	got := delta([]byte("abc"), []byte{3, 4, 0x90, 3, 1, '!'})
	if string(got) != "abc!" {
		t.Fatal(string(got))
	}
}
func TestThreeWayMerge(t *testing.T) {
	r := fixture()
	base := r.resolve("main")
	ours := r.files(base)
	ours["ours.txt"] = Entry{"100644", r.put("blob", []byte("ours"))}
	current := r.commit(r.tree(ours), []string{base}, "ours")
	theirs := r.files(base)
	theirs["theirs.txt"] = Entry{"100644", r.put("blob", []byte("theirs"))}
	head := r.commit(r.tree(theirs), []string{base}, "theirs")
	files, c := r.mergePlan(current, head)
	if len(c) != 0 || len(files) != 3 {
		t.Fatal(files, c)
	}
	theirs["base.txt"] = Entry{"100644", r.put("blob", []byte("other"))}
	head = r.commit(r.tree(theirs), []string{base}, "conflict")
	ours["base.txt"] = Entry{"100644", r.put("blob", []byte("change"))}
	current = r.commit(r.tree(ours), []string{base}, "conflict")
	_, c = r.mergePlan(current, head)
	if len(c) != 1 {
		t.Fatal(c)
	}
}
func TestAtomicQuarantine(t *testing.T) {
	r := fixture()
	base := r.resolve("main")
	o := Object{"blob", []byte("quarantine")}
	id := oid(o)
	p := pack(map[string]Object{id: o}, []string{id})
	var b bytes.Buffer
	b.Write(pkt(base + " " + id + " refs/heads/main\x00report-status atomic\n"))
	b.Write(pkt(zero + " " + base + " refs/heads/valid\n"))
	b.WriteString("0000")
	b.Write(p)
	response := receive(r, b.Bytes())
	if !bytes.Contains(response, []byte("atomic transaction aborted")) || r.resolve("main") != base || r.Refs["refs/heads/valid"] != "" {
		t.Fatal(string(response))
	}
	if _, ok := r.Objects[id]; ok {
		t.Fatal("quarantine leaked")
	}
}
func TestCAS(t *testing.T) {
	r := fixture()
	base := r.resolve("main")
	b := append(pkt(zero+" "+base+" refs/heads/main\x00report-status\n"), []byte("0000")...)
	if !bytes.Contains(receive(r, b), []byte("stale old object id")) {
		t.Fatal("CAS not enforced")
	}
}
func TestReviewExpectedHead(t *testing.T) {
	r := fixture()
	base := r.resolve("main")
	files := r.files(base)
	files["x"] = Entry{"100644", r.put("blob", []byte("x"))}
	head := r.commit(r.tree(files), []string{base}, "feature")
	r.Refs["refs/heads/feature"] = head
	p := r.newPull(M{"head": "feature", "base": "main", "title": "Test"})
	r.Protection["main"] = M{"required_pull_request_reviews": M{"dismiss_stale_reviews": true, "required_approving_review_count": 1}}
	func() {
		defer func() {
			if recover() == nil {
				t.Error("unapproved merge")
			}
		}()
		r.mergePull(p, M{})
	}()
	p["reviews"] = []M{{"state": "APPROVED", "commit_id": head}}
	func() {
		defer func() {
			if recover() == nil {
				t.Error("stale expected head")
			}
		}()
		r.mergePull(p, M{"sha": base})
	}()
	result := r.mergePull(p, M{"sha": head})
	if result["merged"] != true {
		t.Fatal(result)
	}
}
func TestRESTLifecycle(t *testing.T) {
	s := NewStore()
	call := func(method, path, body string) (int, M) {
		q := httptest.NewRequest(method, path, strings.NewReader(body))
		q.Header.Set("Authorization", "Bearer forge-demo")
		w := httptest.NewRecorder()
		s.ServeHTTP(w, q)
		var v M
		json.Unmarshal(w.Body.Bytes(), &v)
		return w.Code, v
	}
	if c, _ := call("GET", "/api/v3/repos/missing/repo", ""); c != 404 {
		t.Fatal(c)
	}
	if c, _ := call("POST", "/api/v3/user/repos", `{"name":"test"}`); c != 201 {
		t.Fatal(c)
	}
	if c, _ := call("PUT", "/api/v3/repos/adaiversen/test/contents/a", `{"content":"aGVsbG8=","message":"file"}`); c != 201 {
		t.Fatal(c)
	}
	if c, v := call("GET", "/api/v3/repos/adaiversen/test/contents/a", ""); c != 200 || v["content"] != "aGVsbG8=" {
		t.Fatal(c, v)
	}
	if c, _ := call("GET", "/api/v3/repos/adaiversen/test/unknown", ""); c != 404 {
		t.Fatal(c)
	}
}
func TestMCPSharedStore(t *testing.T) {
	s := NewStore()
	q := httptest.NewRequest("POST", "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"create_repo","arguments":{"name":"mcp"}}}`))
	q.Header.Set("Authorization", "Bearer forge-demo")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, q)
	if w.Code != 200 || s.repo("adaiversen", "mcp") == nil {
		t.Fatal(w.Body.String())
	}
}
func BenchmarkRepo(b *testing.B) {
	s := NewStore()
	s.Repos["adaiversen/sample"] = fixture()
	q := httptest.NewRequest("GET", "/api/v3/repos/adaiversen/sample", nil)
	q.Header.Set("Authorization", "Bearer forge-demo")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s.ServeHTTP(httptest.NewRecorder(), q)
	}
}
func BenchmarkRefs(b *testing.B) {
	r := fixture()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		advertise(r, "git-upload-pack", true)
	}
}
func BenchmarkCloneCached(b *testing.B) {
	r := fixture()
	raw := append(pkt("command=fetch\n"), []byte("0001")...)
	raw = append(raw, pkt("want "+r.resolve("main")+"\n")...)
	raw = append(raw, pkt("done\n")...)
	raw = append(raw, []byte("0000")...)
	q := httptest.NewRequest("POST", "/adaiversen/sample.git/git-upload-pack", nil)
	q.Header.Set("Git-Protocol", "version=2")
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		q.Body = io.NopCloser(bytes.NewReader(raw))
		gitHTTP(httptest.NewRecorder(), q, r, []string{"git-upload-pack"})
	}
}
func BenchmarkHealth(b *testing.B) {
	s := NewStore()
	q := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		s.ServeHTTP(httptest.NewRecorder(), q)
	}
}
func Example_oid() {
	fmt.Println(oid(Object{"blob", []byte("hello\n")})) // Output: ce013625030ba8dba906f756967f9e9ca394464a
}

func TestRefNamespaceCollision(t *testing.T) {
	r := fixture()
	id := r.resolve("main")
	raw := append(pkt(zero+" "+id+" refs/heads/main/child\x00report-status\n"), []byte("0000")...)
	if !bytes.Contains(receive(r, raw), []byte("namespace collision")) {
		t.Fatal("accepted parent/child refs")
	}
}
func TestReviewSupersedesChanges(t *testing.T) {
	r := fixture()
	root := r.resolve("main")
	r.Refs["refs/heads/feature"] = r.commit(r.tree(r.files(root)), []string{root}, "feature")
	p := r.newPull(M{"title": "Review sequence", "head": "feature", "base": "main"})
	head := r.resolve("feature")
	r.Protection["main"] = M{"required_pull_request_reviews": M{"required_approving_review_count": 1}}
	p["reviews"] = []M{{"state": "CHANGES_REQUESTED", "commit_id": head}, {"state": "APPROVED", "commit_id": head}}
	if r.mergePull(p, M{})["merged"] != true {
		t.Fatal("latest approval did not supersede")
	}
}

// Real TCP requests include HTTP framing, socket I/O and the Go client.
func BenchmarkHTTP(b *testing.B) {
	s := NewStore()
	s.Repos["adaiversen/sample"] = fixture()
	server := httptest.NewServer(s)
	defer server.Close()
	client := server.Client()
	raw := append(pkt("command=fetch\n"), []byte("0001")...)
	raw = append(raw, pkt("want "+s.repo("adaiversen", "sample").resolve("main")+"\n")...)
	raw = append(raw, pkt("done\n")...)
	raw = append(raw, []byte("0000")...)
	for _, test := range []struct {
		name, path, method string
		body               []byte
	}{{"Repo", "/api/v3/repos/adaiversen/sample", "GET", nil}, {"Refs", "/adaiversen/sample.git/info/refs?service=git-upload-pack", "GET", nil}, {"Clone", "/adaiversen/sample.git/git-upload-pack", "POST", raw}} {
		b.Run(test.name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				req, _ := http.NewRequest(test.method, server.URL+test.path, bytes.NewReader(test.body))
				req.Header.Set("Authorization", "Bearer forge-demo")
				req.Header.Set("Git-Protocol", "version=2")
				resp, err := client.Do(req)
				if err != nil {
					b.Fatal(err)
				}
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				if resp.StatusCode != 200 {
					b.Fatal(resp.StatusCode)
				}
			}
		})
	}
}
func TestStreamingPackReader(t *testing.T) {
	r := fixture()
	ids := []string{}
	for id := range r.Objects {
		ids = append(ids, id)
	}
	reader, writer := io.Pipe()
	go func() {
		defer writer.Close()
		for _, b := range pack(r.Objects, ids) {
			writer.Write([]byte{b})
		}
	}()
	got := parsePackReader(reader, nil)
	if len(got) != len(ids) {
		t.Fatal("stream lost objects")
	}
}
