package main

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type M = map[string]any
type Repo struct {
	sync.RWMutex
	ID                       int
	Name, Owner, Description string
	Private                  bool
	Refs                     map[string]string
	Objects                  map[string]Object
	Issues                   []M
	PRs                      []M
	Labels                   map[string]M
	Protection               map[string]M
	Version                  int
	Cache                    map[string][]byte
	Graph                    map[string]map[string]bool
}
type Store struct {
	sync.RWMutex
	Repos map[string]*Repo
	Next  int
}

func NewStore() *Store { return &Store{Repos: map[string]*Repo{}} }
func (s *Store) create(o, n string) *Repo {
	s.Lock()
	defer s.Unlock()
	if n == "" || strings.ContainsAny(n, " /\\") {
		panic(Fault{422, "Invalid name"})
	}
	key := o + "/" + n
	if s.Repos[key] != nil {
		panic(Fault{422, "Already exists"})
	}
	s.Next++
	r := &Repo{ID: s.Next, Name: n, Owner: o, Refs: map[string]string{}, Objects: map[string]Object{}, Labels: map[string]M{}, Protection: map[string]M{}, Cache: map[string][]byte{}, Issues: []M{}, PRs: []M{}}
	s.Repos[key] = r
	return r
}
func (s *Store) repo(o, n string) *Repo {
	s.RLock()
	defer s.RUnlock()
	r := s.Repos[o+"/"+n]
	if r == nil {
		panic(Fault{404, "Not Found"})
	}
	return r
}

type Fault struct {
	Code    int
	Message string
}

func fail(code int, msg string)      { panic(Fault{code, msg}) }
func str(m M, k string) string       { v, _ := m[k].(string); return v }
func j(w http.ResponseWriter, v any) { answer(w, 200, v) }
func answer(w http.ResponseWriter, code int, v any) {
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(code)
	if code != 204 {
		json.NewEncoder(w).Encode(v)
	}
}
func (r *Repo) json() M {
	return M{"id": r.ID, "node_id": fmt.Sprintf("Repository:%d", r.ID), "name": r.Name, "full_name": r.Owner + "/" + r.Name, "owner": M{"login": r.Owner}, "description": r.Description, "private": r.Private, "default_branch": "main", "created_at": "2026-11-17T00:00:00Z", "updated_at": "2026-11-17T00:00:00Z"}
}
func (r *Repo) touch() { r.Version++; r.Cache = map[string][]byte{} }
func (s *Store) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	defer func() {
		if e := recover(); e != nil {
			if f, ok := e.(Fault); ok {
				answer(w, f.Code, M{"message": f.Message})
			} else {
				answer(w, 400, M{"message": fmt.Sprint(e)})
			}
		}
	}()
	if r.Header.Get("Content-Encoding") == "gzip" {
		gz, e := gzip.NewReader(r.Body)
		if e != nil {
			fail(400, "Invalid gzip body")
		}
		defer gz.Close()
		r.Body = io.NopCloser(gz)
	}
	w.Header().Set("X-GitHub-Request-Id", "GO-FORGE-4")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if r.URL.Path == "/healthz" {
		j(w, M{"ok": true})
		return
	}
	auth := r.Header.Get("Authorization")
	token := os.Getenv("MAKEFUN_SHOP_TOKEN")
	if token == "" {
		token = "forge-demo"
	}
	ok := auth == "Bearer "+token || auth == "token "+token
	if strings.HasPrefix(auth, "Basic ") {
		_, pass, yes := r.BasicAuth()
		ok = yes && pass == token
	}
	p := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	isGit := len(p) >= 3 && strings.HasSuffix(p[1], ".git")
	if (strings.HasPrefix(r.URL.Path, "/api/") || r.URL.Path == "/mcp" || isGit && strings.Contains(r.URL.String(), "receive-pack")) && !ok {
		w.Header().Set("WWW-Authenticate", "Basic realm=forge")
		fail(401, "Bad credentials")
	}
	if isGit {
		repo := s.repo(p[0], strings.TrimSuffix(p[1], ".git"))
		if repo.Private && !ok {
			fail(401, "Bad credentials")
		}
		repo.Lock()
		defer repo.Unlock()
		serveGitAdapter(w, r, repo)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/api/") || r.URL.Path == "/mcp" {
		in := M{}
		if r.Body != nil && r.Method != "GET" {
			b, err := io.ReadAll(io.LimitReader(r.Body, 64<<20))
			if err != nil {
				fail(400, "Read error")
			}
			if r.URL.Path == "/api/graphql" && len(b) > 0 && b[0] == '[' {
				var batch []M
				if json.Unmarshal(b, &batch) != nil {
					fail(400, "Malformed JSON")
				}
				out := []M{}
				for _, op := range batch {
					out = append(out, s.graphql(op))
				}
				j(w, out)
				return
			}
			if len(b) > 0 && json.Unmarshal(b, &in) != nil {
				if r.URL.Path == "/mcp" {
					answer(w, 400, M{"jsonrpc": "2.0", "id": nil, "error": M{"code": -32700, "message": "Parse error"}})
					return
				}
				fail(400, "Malformed JSON")
			}
		}
		if r.URL.Path == "/api/graphql" {
			j(w, s.graphql(in))
			return
		}
		if r.URL.Path == "/mcp" {
			s.mcp(w, r, in)
			return
		}
		s.api(w, r, p[2:], in)
		return
	}
	s.ui(w, r, p)
}
func main() {
	if dir := os.Getenv("MAKEFUN_OUTPUT_DIR"); dir != "" && os.Getenv("MAKEFUN_ENV_URL") == "" {
		writeProof(dir)
		return
	}
	s := NewStore()
	port := os.Getenv("PORT")
	if port == "" {
		port = "0"
	}
	ln, err := net.Listen("tcp", ":"+port)
	if err != nil {
		panic(err)
	}
	srv := &http.Server{Handler: s, ReadHeaderTimeout: 10 * time.Second}
	go srv.Serve(ln)
	fmt.Println("forge listening", ln.Addr())
	env := strings.TrimRight(os.Getenv("MAKEFUN_ENV_URL"), "/")
	if env == "" {
		select {}
	}
	client := &http.Client{Timeout: 30 * time.Second}
	data, _ := json.Marshal(M{"port": ln.Addr().(*net.TCPAddr).Port})
	for {
		resp, e := client.Post(env+"/register", "application/json", bytes.NewReader(data))
		if e == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 || resp.StatusCode == 409 {
				break
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	for {
		resp, e := client.Get(env + "/done")
		if e == nil {
			var v M
			json.NewDecoder(resp.Body).Decode(&v)
			resp.Body.Close()
			if v["done"] == true {
				break
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	srv.Close()
}

// The offline artifact executes the same object/merge engine used by HTTP.
func writeProof(dir string) {
	s := NewStore()
	r := s.create("forge", "proof")
	rootFiles := map[string]Entry{"README.md": {"100644", r.put("blob", []byte("# Go Forge\n"))}}
	root := r.commit(r.tree(rootFiles), nil, "Initial proof")
	ours := r.files(root)
	ours["base.txt"] = Entry{"100644", r.put("blob", []byte("base change\n"))}
	base := r.commit(r.tree(ours), []string{root}, "Base advances")
	theirs := r.files(root)
	theirs["feature.txt"] = Entry{"100644", r.put("blob", []byte("feature change\n"))}
	head := r.commit(r.tree(theirs), []string{root}, "Feature advances")
	files, conflicts := r.mergePlan(base, head)
	if len(conflicts) != 0 {
		panic("offline merge failed")
	}
	merged := r.commit(r.tree(files), []string{base, head}, "Merge proof")
	r.Refs["refs/heads/main"] = merged
	r.Refs["refs/heads/feature"] = head
	proof := M{"format": "go-forge/proof/v4", "repository": r.json(), "refs": r.Refs, "objects": r.Objects, "merged_files": r.files(merged), "conflicts": conflicts}
	raw, err := json.MarshalIndent(proof, "", "  ")
	if err != nil {
		panic(err)
	}
	if err = os.MkdirAll(dir, 0755); err != nil {
		panic(err)
	}
	if err = os.WriteFile(filepath.Join(dir, "forge.json"), append(raw, '\n'), 0644); err != nil {
		panic(err)
	}
}
