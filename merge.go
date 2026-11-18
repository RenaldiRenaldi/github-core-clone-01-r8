package main

import (
	"fmt"
	"sort"
	"strings"
)

func (r *Repo) mergePlan(base, head string) (map[string]Entry, []string) {
	common := ""
	q := []string{head}
	seen := map[string]bool{}
	for len(q) > 0 {
		id := q[0]
		q = q[1:]
		if seen[id] {
			continue
		}
		seen[id] = true
		if r.history(base)[id] {
			common = id
			break
		}
		_, p := info(r.Objects[id])
		q = append(q, p...)
	}
	old := map[string]Entry{}
	if common != "" {
		old = r.files(common)
	}
	ours, theirs := r.files(base), r.files(head)
	result := map[string]Entry{}
	for p, e := range ours {
		result[p] = e
	}
	paths := map[string]bool{}
	for p := range old {
		paths[p] = true
	}
	for p := range theirs {
		paths[p] = true
	}
	conflicts := []string{}
	for p := range paths {
		a, ao := old[p]
		b, bo := ours[p]
		c, co := theirs[p]
		if a == c && ao == co || b == c && bo == co {
			continue
		}
		if a != b || ao != bo {
			conflicts = append(conflicts, p)
			continue
		}
		if co {
			result[p] = c
		} else {
			delete(result, p)
		}
	}
	for p := range result {
		for other := range result {
			if strings.HasPrefix(other, p+"/") {
				conflicts = append(conflicts, p)
			}
		}
	}
	return result, conflicts
}
func (r *Repo) pullJSON(p M) M {
	out := M{}
	for k, v := range p {
		out[k] = v
	}
	head, base := str(p, "head_ref"), str(p, "base_ref")
	out["head"] = M{"ref": head, "sha": r.resolve(head), "repo": r.json()}
	out["base"] = M{"ref": base, "sha": r.resolve(base), "repo": r.json()}
	_, c := r.mergePlan(r.resolve(base), r.resolve(head))
	out["mergeable"] = len(c) == 0
	return out
}
func (r *Repo) newPull(in M) M {
	h, b := str(in, "head"), str(in, "base")
	if strings.Contains(h, ":") {
		h = strings.Split(h, ":")[1]
	}
	if h == b || h == "" || b == "" {
		fail(422, "Distinct head and base required")
	}
	r.resolve(h)
	r.resolve(b)
	p := r.newIssue(in)
	p["head_ref"] = h
	p["base_ref"] = b
	p["merged"] = false
	p["reviews"] = []M{}
	r.Issues = r.Issues[:len(r.Issues)-1]
	r.PRs = append(r.PRs, p)
	r.touch()
	return p
}
func (r *Repo) newIssue(in M) M {
	if str(in, "title") == "" {
		fail(422, "title required")
	}
	labels := []M{}
	if ls, ok := in["labels"].([]any); ok {
		for _, l := range ls {
			name, _ := l.(string)
			label := r.Labels[name]
			if label == nil {
				fail(422, "Unknown label")
			}
			labels = append(labels, label)
		}
	}
	n := len(r.Issues) + len(r.PRs) + 1
	p := M{"id": r.ID*100000 + n, "number": n, "node_id": fmt.Sprintf("Issue:%d:%d", r.ID, n), "title": str(in, "title"), "body": str(in, "body"), "state": "open", "labels": labels, "user": M{"login": "adaiversen"}, "comments": 0, "created_at": "2026-11-17T00:00:00Z", "updated_at": "2026-11-17T00:00:00Z", "comments_list": []M{}}
	r.Issues = append(r.Issues, p)
	r.touch()
	return p
}
func (r *Repo) mergePull(p M, in M) M {
	if p["merged"] == true {
		fail(405, "Already merged")
	}
	head, base := r.resolve(str(p, "head_ref")), r.resolve(str(p, "base_ref"))
	if sha := str(in, "sha"); sha != "" && sha != head {
		fail(409, "Head changed")
	}
	settings := r.Protection[str(p, "base_ref")]
	if settings != nil {
		if rv, ok := settings["required_pull_request_reviews"].(map[string]any); ok {
			latest := map[string]M{}
			for _, review := range p["reviews"].([]M) {
				if review["state"] == "COMMENTED" || review["state"] == "DISMISSED" {
					continue
				}
				if rv["dismiss_stale_reviews"] == true && review["commit_id"] != head {
					continue
				}
				login := "adaiversen"
				if user, ok := review["user"].(M); ok && str(user, "login") != "" {
					login = str(user, "login")
				}
				latest[login] = review
			}
			approvals := 0
			for _, review := range latest {
				if review["state"] == "CHANGES_REQUESTED" {
					fail(405, "Changes requested")
				}
				if review["state"] == "APPROVED" {
					approvals++
				}
			}
			required := integer(rv["required_approving_review_count"])
			if required == 0 {
				required = 1
			}
			if approvals < required {
				fail(405, "Approval required")
			}
		}
	}
	files, conflicts := r.mergePlan(base, head)
	if len(conflicts) > 0 {
		fail(405, "Merge conflict")
	}
	parents := []string{base}
	if str(in, "merge_method") == "merge" {
		parents = append(parents, head)
	}
	sha := r.commit(r.tree(files), parents, "Merge pull request: "+str(p, "title"))
	r.Refs["refs/heads/"+str(p, "base_ref")] = sha
	p["merged_at"] = "2026-11-17T00:00:00Z"
	p["state"] = "closed"
	p["merged"] = true
	p["merge_commit_sha"] = sha
	r.touch()
	return M{"sha": sha, "merged": true, "message": "Pull request successfully merged"}
}

// diff compares immutable tree snapshots; adapters share these exact changes.
func (r *Repo) diff(base, head string) []M {
	before, after := r.files(base), r.files(head)
	paths := map[string]bool{}
	for p := range before {
		paths[p] = true
	}
	for p := range after {
		paths[p] = true
	}
	out := []M{}
	for p := range paths {
		a, ao := before[p]
		b, bo := after[p]
		if a == b && ao == bo {
			continue
		}
		status := "modified"
		if !ao {
			status = "added"
		}
		if !bo {
			status = "removed"
		}
		old, next := string(r.Objects[a.SHA].Data), string(r.Objects[b.SHA].Data)
		adds, dels := strings.Count(next, "\n"), strings.Count(old, "\n")
		out = append(out, M{"filename": p, "path": p, "status": status, "sha": b.SHA, "additions": adds, "deletions": dels, "changes": adds + dels, "patch": "@@ -1 +1 @@\n-" + old + "+" + next})
	}
	sort.Slice(out, func(i, j int) bool { return str(out[i], "filename") < str(out[j], "filename") })
	return out
}
