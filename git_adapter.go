package main

import (
	"net/http"
	"strings"
)

// serveGitAdapter runs the existing smart-HTTP implementation against the
// selected repository's object and ref maps, then publishes accepted pushes
// back into the shared REST/GraphQL store.
func serveGitAdapter(w http.ResponseWriter, r *http.Request, repo *Repo) {
	core := &r8Forge{
		objects: make(map[string]r8Object, len(repo.Objects)),
		refs:    make(map[string]string, len(repo.Refs)),
		cache:   make(map[string][]byte),
	}
	for id, obj := range repo.Objects {
		core.objects[id] = r8Object{kind: obj.Type, data: obj.Data}
	}
	for ref, id := range repo.Refs {
		core.refs[ref] = id
	}

	originalPath := r.URL.Path
	prefix := "/" + repo.Owner + "/" + repo.Name + ".git"
	if !strings.HasPrefix(originalPath, prefix+"/") {
		http.NotFound(w, r)
		return
	}
	core.ServeHTTP(w, r)

	for ref := range repo.Refs {
		delete(repo.Refs, ref)
	}
	for ref, id := range core.refs {
		repo.Refs[ref] = id
	}
	for id, obj := range core.objects {
		repo.Objects[id] = Object{Type: obj.kind, Data: obj.data}
	}
	repo.touch()
}
