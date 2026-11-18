package main

import (
	"fmt"
	"net/http"
	"strings"
)

func pkt(s string) []byte { return []byte(fmt.Sprintf("%04x%s", len(s)+4, s)) }

// serveGitAdapter keeps repository routing in the API store while the Git
// transport and object maps remain shared with REST and GraphQL.
func serveGitAdapter(w http.ResponseWriter, r *http.Request, repo *Repo) {
	prefix := "/" + repo.Owner + "/" + repo.Name + ".git/"
	if !strings.HasPrefix(r.URL.Path, prefix) {
		http.NotFound(w, r)
		return
	}
	gitHTTP(w, r, repo, strings.Split(strings.TrimPrefix(r.URL.Path, prefix), "/"))
}
