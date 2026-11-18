package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
)

var toolSpecs = []struct {
	Name, Desc string
	Props      []string
	Required   []string
}{{"create_repo", "Create a repository", []string{"name"}, []string{"name"}}, {"get_repo", "Read a repository", []string{"owner", "repo"}, []string{"owner", "repo"}}, {"create_issue", "Open an issue", []string{"owner", "repo", "title", "body"}, []string{"owner", "repo", "title"}}, {"list_issues", "List issues", []string{"owner", "repo", "state"}, []string{"owner", "repo"}}, {"create_pull_request", "Open a pull request", []string{"owner", "repo", "title", "head", "base"}, []string{"owner", "repo", "title", "head", "base"}}, {"merge_pull_request", "Merge a reviewed pull request", []string{"owner", "repo", "pull_number", "sha"}, []string{"owner", "repo", "pull_number"}}, {"review_pull_request", "Review a pull request", []string{"owner", "repo", "pull_number", "event", "body"}, []string{"owner", "repo", "pull_number", "event"}}, {"get_file_contents", "Read file contents", []string{"owner", "repo", "path", "ref"}, []string{"owner", "repo", "path"}}}

func (s *Store) mcp(w http.ResponseWriter, q *http.Request, in M) {
	result := M{}
	response := M{"jsonrpc": "2.0", "id": in["id"]}
	params, _ := in["params"].(M)
	if params == nil {
		params = M{}
	}
	switch str(in, "method") {
	case "notifications/initialized":
		answer(w, 204, nil)
		return
	case "initialize":
		result = M{"protocolVersion": "2025-11-25", "serverInfo": M{"name": "Go Forge", "version": "4"}, "capabilities": M{"tools": M{}}}
	case "server/discover":
		result = M{"capabilities": M{"tools": M{}}, "protocolVersion": "2026-07-28", "supportedVersions": []string{"2026-07-28", "2025-11-25"}, "serverInfo": M{"name": "Go Forge", "version": "4"}}
	case "tools/list":
		tools := []M{}
		for _, t := range toolSpecs {
			props := M{}
			for _, p := range t.Props {
				typ := "string"
				if p == "pull_number" {
					typ = "integer"
				}
				props[p] = M{"type": typ}
			}
			tools = append(tools, M{"name": t.Name, "description": t.Desc, "inputSchema": M{"type": "object", "properties": props, "required": t.Required}})
		}
		result = M{"tools": tools}
	case "tools/call":
		name := str(params, "name")
		args, _ := params["arguments"].(M)
		if args == nil {
			args = M{}
		}
		found := false
		for _, t := range toolSpecs {
			if name == t.Name {
				found = true
				for _, required := range t.Required {
					if args[required] == nil || args[required] == "" {
						response["error"] = M{"code": -32602, "message": "Missing " + required}
						j(w, response)
						return
					}
				}
			}
		}
		if !found {
			response["error"] = M{"code": -32602, "message": "Unknown tool"}
			j(w, response)
			return
		}
		method, path := "GET", "/api/v3/repos/"+str(args, "owner")+"/"+str(args, "repo")
		switch name {
		case "create_repo":
			method, path = "POST", "/api/v3/user/repos"
		case "create_issue":
			method = "POST"
			path += "/issues"
		case "list_issues":
			path += "/issues?state=" + str(args, "state")
		case "create_pull_request":
			method = "POST"
			path += "/pulls"
		case "merge_pull_request":
			method = "PUT"
			path += fmt.Sprintf("/pulls/%d/merge", integer(args["pull_number"]))
		case "review_pull_request":
			method = "POST"
			path += fmt.Sprintf("/pulls/%d/reviews", integer(args["pull_number"]))
		case "get_file_contents":
			path += "/contents/" + str(args, "path") + "?ref=" + str(args, "ref")
		}
		b, _ := json.Marshal(args)
		req := httptest.NewRequest(method, path, bytes.NewReader(b))
		req.Header = q.Header.Clone()
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		result = M{"content": []M{{"type": "text", "text": rec.Body.String()}}, "isError": rec.Code >= 400}
	default:
		response["error"] = M{"code": -32601, "message": "Unknown method"}
		j(w, response)
		return
	}
	response["result"] = result
	j(w, response)
}
