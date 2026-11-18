package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
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
	ok := authorized(r)
	p := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	isGit := len(p) >= 3 && strings.HasSuffix(p[1], ".git")
	writeGit := isGit && (p[2] == "git-receive-pack" || r.URL.Query().Get("service") == "git-receive-pack")
	if (strings.HasPrefix(r.URL.Path, "/api/") || r.URL.Path == "/mcp" || writeGit) && !ok {
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
			b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<20))
			if err != nil {
				fail(400, "Read error")
			}
			b = bytes.TrimSpace(b)
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
func authorized(request *http.Request) bool {
	want := os.Getenv("MAKEFUN_SHOP_TOKEN")
	if want == "" {
		want = "forge-demo"
	}
	var credential string
	if _, password, basic := request.BasicAuth(); basic {
		credential = password
	} else {
		scheme, value, present := strings.Cut(request.Header.Get("Authorization"), " ")
		if !present || !(strings.EqualFold(scheme, "bearer") || strings.EqualFold(scheme, "token")) {
			return false
		}
		credential = value
	}
	return subtle.ConstantTimeCompare([]byte(credential), []byte(want)) == 1
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := runForge(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "forge:", err)
		os.Exit(1)
	}
}

func runForge(ctx context.Context) error {
	controller := strings.TrimRight(os.Getenv("MAKEFUN_ENV_URL"), "/")
	if output := os.Getenv("MAKEFUN_OUTPUT_DIR"); output != "" && controller == "" {
		writeProof(output)
		return nil
	}
	listenPort := os.Getenv("PORT")
	if listenPort == "" {
		listenPort = "0"
	}
	listener, listenErr := net.Listen("tcp", net.JoinHostPort("", listenPort))
	if listenErr != nil {
		return fmt.Errorf("listen on port %s: %w", listenPort, listenErr)
	}
	actual := listener.Addr().(*net.TCPAddr).Port
	service := &http.Server{
		Handler:           NewStore(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	defer service.Close()
	stopped := make(chan error, 1)
	go func() { stopped <- service.Serve(listener) }()
	fmt.Printf("forge listening on port %d\n", actual)

	if controller != "" {
		go func() {
			followController(ctx, controller, actual)
			stopped <- nil
		}()
	}
	select {
	case serveErr := <-stopped:
		return serveErr
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		return service.Shutdown(shutdown)
	}
}

func followController(ctx context.Context, base string, port int) {
	client := &http.Client{Timeout: 30 * time.Second}
	defer client.CloseIdleConnections()
	address := []byte(fmt.Sprintf("{\"port\":%d}", port))
	poll := time.NewTicker(250 * time.Millisecond)
	defer poll.Stop()
	registered := false
	for {
		if registered {
			if controllerDone(ctx, client, base) {
				return
			}
		} else {
			registered = registerServer(ctx, client, base, address)
			if registered {
				continue
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-poll.C:
		}
	}
}

func registerServer(ctx context.Context, client *http.Client, base string, body []byte) bool {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/register", bytes.NewReader(body))
	if err != nil {
		return false
	}
	request.Header.Set("Content-Type", "application/json")
	reply, err := client.Do(request)
	if err != nil {
		return false
	}
	defer reply.Body.Close()
	io.Copy(io.Discard, io.LimitReader(reply.Body, 4096))
	return reply.StatusCode == http.StatusOK || reply.StatusCode == http.StatusConflict
}

func controllerDone(ctx context.Context, client *http.Client, base string) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/done", nil)
	if err != nil {
		return false
	}
	res, err := client.Do(req)
	if err != nil {
		return false
	}
	defer res.Body.Close()
	var progress struct {
		Done bool `json:"done"`
	}
	return json.NewDecoder(io.LimitReader(res.Body, 4096)).Decode(&progress) == nil && progress.Done
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
