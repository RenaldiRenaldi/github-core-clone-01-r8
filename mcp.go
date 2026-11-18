package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
)

type mcpTool struct {
	name        string
	description string
	required    []string
	optional    []string
	method      string
	resource    string
}

var mcpTools = []mcpTool{
	{name: "create_repo", description: "Create a repository", required: []string{"name"}, method: http.MethodPost},
	{name: "get_repo", description: "Read repository metadata", required: []string{"owner", "repo"}, method: http.MethodGet},
	{name: "create_issue", description: "Open an issue", required: []string{"owner", "repo", "title"}, optional: []string{"body"}, method: http.MethodPost, resource: "issues"},
	{name: "list_issues", description: "List repository issues", required: []string{"owner", "repo"}, optional: []string{"state"}, method: http.MethodGet, resource: "issues"},
	{name: "create_pull_request", description: "Open a pull request", required: []string{"owner", "repo", "title", "head", "base"}, method: http.MethodPost, resource: "pulls"},
	{name: "merge_pull_request", description: "Merge a pull request", required: []string{"owner", "repo", "pull_number"}, optional: []string{"sha"}, method: http.MethodPut, resource: "merge"},
	{name: "review_pull_request", description: "Review a pull request", required: []string{"owner", "repo", "pull_number", "event"}, optional: []string{"body"}, method: http.MethodPost, resource: "reviews"},
	{name: "get_file_contents", description: "Read a file at a branch, tag, or commit", required: []string{"owner", "repo", "path"}, optional: []string{"ref"}, method: http.MethodGet, resource: "contents"},
}

func (tool mcpTool) schema() M {
	properties := make(M, len(tool.required)+len(tool.optional))
	add := func(names []string) {
		for _, parameter := range names {
			kind := "string"
			if parameter == "pull_number" {
				kind = "integer"
			}
			properties[parameter] = M{"type": kind}
		}
	}
	add(tool.required)
	add(tool.optional)
	return M{
		"name": tool.name, "description": tool.description,
		"inputSchema": M{"type": "object", "properties": properties, "required": tool.required},
	}
}

func (tool mcpTool) endpoint(args M) string {
	if tool.name == "create_repo" {
		return "/api/v3/user/repos"
	}
	owner := url.PathEscape(str(args, "owner"))
	repo := url.PathEscape(str(args, "repo"))
	root := "/api/v3/repos/" + owner + "/" + repo
	query := url.Values{}
	switch tool.resource {
	case "":
		// Repository metadata is served at the root of the REST resource.
	case "issues":
		root += "/issues"
		if state := str(args, "state"); state != "" {
			query.Set("state", state)
		}
	case "contents":
		segments := strings.Split(str(args, "path"), "/")
		for index, segment := range segments {
			segments[index] = url.PathEscape(segment)
		}
		root += "/contents/" + strings.Join(segments, "/")
		if ref := str(args, "ref"); ref != "" {
			query.Set("ref", ref)
		}
	case "merge", "reviews":
		root += fmt.Sprintf("/pulls/%d/%s", integer(args["pull_number"]), tool.resource)
	default:
		root += "/" + tool.resource
	}
	if encoded := query.Encode(); encoded != "" {
		root += "?" + encoded
	}
	return root
}

func (s *Store) mcp(w http.ResponseWriter, request *http.Request, message M) {
	method := str(message, "method")
	if method == "notifications/initialized" {
		answer(w, http.StatusNoContent, nil)
		return
	}
	reply := M{"jsonrpc": "2.0", "id": message["id"]}
	server := func(version string) M {
		return M{
			"protocolVersion": version,
			"serverInfo":      M{"name": "Go Forge", "version": "8"},
			"capabilities":    M{"tools": M{}},
		}
	}
	switch method {
	case "initialize":
		reply["result"] = server("2025-11-25")
	case "server/discover":
		discovery := server("2026-07-28")
		discovery["supportedVersions"] = []string{"2026-07-28", "2025-11-25"}
		reply["result"] = discovery
	case "tools/list":
		schemas := make([]M, 0, len(mcpTools))
		for _, tool := range mcpTools {
			schemas = append(schemas, tool.schema())
		}
		reply["result"] = M{"tools": schemas}
	case "tools/call":
		params, _ := message["params"].(M)
		result, diagnostic := s.callMCPTool(request, params)
		if diagnostic != "" {
			reply["error"] = M{"code": -32602, "message": diagnostic}
		} else {
			reply["result"] = result
		}
	default:
		reply["error"] = M{"code": -32601, "message": "Unknown method"}
	}
	j(w, reply)
}

func (s *Store) callMCPTool(request *http.Request, params M) (M, string) {
	name := str(params, "name")
	args, _ := params["arguments"].(M)
	for _, tool := range mcpTools {
		if tool.name != name {
			continue
		}
		for _, key := range tool.required {
			if key == "pull_number" {
				if integer(args[key]) <= 0 {
					return nil, "Missing or invalid " + key
				}
			} else if str(args, key) == "" {
				return nil, "Missing " + key
			}
		}
		body, err := json.Marshal(args)
		if err != nil {
			return nil, "Invalid arguments"
		}
		// REST owns validation and mutation. Carry over authentication, but
		// do not inherit encoding or content negotiation from the MCP envelope.
		sub := httptest.NewRequestWithContext(request.Context(), tool.method, tool.endpoint(args), bytes.NewReader(body))
		sub.Header.Set("Authorization", request.Header.Get("Authorization"))
		sub.Header.Set("Content-Type", "application/json")
		capture := httptest.NewRecorder()
		s.ServeHTTP(capture, sub)
		return M{
			"content": []M{{"type": "text", "text": capture.Body.String()}},
			"isError": capture.Code >= http.StatusBadRequest,
		}, ""
	}
	return nil, "Unknown tool"
}
