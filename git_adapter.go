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
	suffix, matched := strings.CutPrefix(r.URL.Path, prefix)
	if !matched {
		http.NotFound(w, r)
		return
	}
	switch suffix {
	case "info/refs", "git-upload-pack", "git-receive-pack":
		gitHTTP(w, r, repo, strings.Split(suffix, "/"))
	default:
		http.NotFound(w, r)
	}
}
