package main

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
)

// ---------- MCP (JSON-RPC 2.0 Streamable HTTP, 2026-07-28) ----------

var toolsListJSON []byte
var toolsListOnce sync.Once

func buildToolsList() {
	toolsListOnce.Do(func() {
		strProp := map[string]any{"type": "string"}
		boolProp := map[string]any{"type": "boolean"}
		tools := []map[string]any{
			{
				"name": "create_repo",
				"description": "Create a new repository owned by the viewer.",
				"inputSchema": map[string]any{"type": "object", "properties": map[string]any{
					"name": strProp, "description": strProp, "private": boolProp, "owner": strProp,
				}, "required": []string{"name"}},
			},
			{
				"name": "get_repo",
				"description": "Get a repository by owner and name.",
				"inputSchema": map[string]any{"type": "object", "properties": map[string]any{
					"owner": strProp, "repo": strProp,
				}, "required": []string{"owner", "repo"}},
			},
			{
				"name": "create_issue",
				"description": "Create an issue in a repository.",
				"inputSchema": map[string]any{"type": "object", "properties": map[string]any{
					"owner": strProp, "repo": strProp, "title": strProp, "body": strProp,
				}, "required": []string{"owner", "repo", "title"}},
			},
			{
				"name": "list_issues",
				"description": "List issues in a repository.",
				"inputSchema": map[string]any{"type": "object", "properties": map[string]any{
					"owner": strProp, "repo": strProp, "state": strProp,
				}, "required": []string{"owner", "repo"}},
			},
			{
				"name": "create_pull_request",
				"description": "Create a pull request in a repository.",
				"inputSchema": map[string]any{"type": "object", "properties": map[string]any{
					"owner": strProp, "repo": strProp, "title": strProp, "head": strProp, "base": strProp, "body": strProp,
				}, "required": []string{"owner", "repo", "title", "head", "base"}},
			},
			{
				"name": "merge_pull_request",
				"description": "Merge a pull request with squash merge.",
				"inputSchema": map[string]any{"type": "object", "properties": map[string]any{
					"owner": strProp, "repo": strProp, "pull_number": map[string]any{"type": "number"},
				}, "required": []string{"owner", "repo", "pull_number"}},
			},
		}
		toolsListJSON, _ = json.Marshal(map[string]any{"tools": tools})
	})
}

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

func rpcError(id json.RawMessage, code int, msg string) []byte {
	if id == nil {
		id = []byte("null")
	}
	b, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": json.RawMessage(id),
		"error": map[string]any{"code": code, "message": msg},
	})
	return b
}

func rpcResult(id json.RawMessage, result any) []byte {
	if id == nil {
		id = []byte("null")
	}
	b, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": json.RawMessage(id), "result": result,
	})
	return b
}

func (s *Store) handleMCP(w http.ResponseWriter, r *http.Request) {
	if r.Method == "DELETE" {
		w.WriteHeader(204)
		return
	}
	if r.Method != "POST" {
		w.WriteHeader(405)
		return
	}
	// auth BEFORE tool lookup
	if !s.gitAuth(r) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(401)
		w.Write([]byte(`{"message":"Requires authentication"}`))
		return
	}
	b, err := io.ReadAll(r.Body)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(400)
		w.Write(rpcError(nil, -32700, "Parse error"))
		return
	}
	var req rpcRequest
	if err := json.Unmarshal(b, &req); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(400)
		w.Write(rpcError(nil, -32700, "Parse error"))
		return
	}
	// notification without id -> 202 empty
	if req.ID == nil || len(req.ID) == 0 {
		w.WriteHeader(202)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	switch req.Method {
	case "server/discover":
		w.WriteHeader(200)
		w.Write(rpcResult(req.ID, map[string]any{
			"supportedVersions": []string{"2026-07-28", "2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"},
			"capabilities":      map[string]any{"tools": map[string]any{}},
			"serverInfo":        map[string]string{"name": "forge-go", "version": "1.0.0"},
			"_meta": map[string]any{"io.modelcontextprotocol/serverInfo": map[string]string{"name": "forge-go", "version": "1.0.0"}},
		}))
	case "initialize":
		var params struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(req.Params, &params)
		ver := params.ProtocolVersion
		supported := map[string]bool{"2026-07-28": true, "2025-11-25": true, "2025-06-18": true, "2025-03-26": true, "2024-11-05": true}
		if !supported[ver] {
			ver = "2025-11-25"
		}
		w.Header().Set("Mcp-Session-Id", "mcp-session-1")
		w.WriteHeader(200)
		w.Write(rpcResult(req.ID, map[string]any{
			"protocolVersion": ver,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]string{"name": "forge-go", "version": "1.0.0"},
		}))
	case "notifications/initialized":
		w.WriteHeader(202)
	case "ping":
		w.WriteHeader(200)
		w.Write(rpcResult(req.ID, map[string]any{}))
	case "tools/list":
		buildToolsList()
		w.WriteHeader(200)
		// tools array is byte-identical across calls (pre-serialized)
		out, _ := json.Marshal(map[string]any{
			"jsonrpc": "2.0", "id": json.RawMessage(req.ID), "result": json.RawMessage(toolsListJSON),
		})
		w.Write(out)
	case "tools/call":
		w.WriteHeader(200)
		w.Write(s.mcpCall(req))
	default:
		w.WriteHeader(200)
		w.Write(rpcError(req.ID, -32601, "Method not found: "+req.Method))
	}
}

func (s *Store) mcpCall(req rpcRequest) []byte {
	var params struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return rpcError(req.ID, -32602, "Invalid params")
	}
	name := strings.ToLower(params.Name)
	// suffix match (tool names may carry a prefix)
	match := func(want string) bool {
		return name == want || strings.HasSuffix(name, "_"+want) || strings.HasSuffix(name, want)
	}
	args := params.Arguments
	strArg := func(keys ...string) string {
		for _, k := range keys {
			if v, ok := args[k].(string); ok && v != "" {
				return v
			}
		}
		return ""
	}
	numArg := func(keys ...string) float64 {
		for _, k := range keys {
			switch v := args[k].(type) {
			case float64:
				return v
			case string:
				f, _ := strconv.ParseFloat(v, 64)
				return f
			}
		}
		return 0
	}
	has := func(keys ...string) bool {
		for _, k := range keys {
			if v, ok := args[k]; ok && v != nil && v != "" {
				return true
			}
		}
		return false
	}
	fail := func(msg string) []byte {
		res := map[string]any{
			"content": []map[string]any{{"type": "text", "text": msg}},
			"structuredContent": map[string]any{"error": msg},
			"isError": true,
		}
		return rpcResult(req.ID, res)
	}
	okResult := func(v any) []byte {
		text, _ := json.Marshal(v)
		res := map[string]any{
			"content":           []map[string]any{{"type": "text", "text": string(text)}},
			"structuredContent": v,
		}
		return rpcResult(req.ID, res)
	}

	switch {
	case match("create_repo") || match("create_repository"):
		if !has("name") {
			return fail("missing required argument: name")
		}
		owner := strArg("owner", "org", "login")
		if owner == "" {
			owner = ViewerLogin
		}
		ownerID := ViewerID
		if owner == "olgabauer" {
			ownerID = StrangerID
		}
		desc := strArg("description", "body")
		priv, _ := args["private"].(bool)
		s.mu.Lock()
		repo, ok := s.createRepo(owner, ownerID, strArg("name"), priv, desc)
		s.mu.Unlock()
		if !ok {
			return fail("Repository already exists")
		}
		s.mu.RLock()
		j := repo.json()
		s.mu.RUnlock()
		return okResult(j)
	case match("get_repo") || match("get_repository"):
		if !has("owner", "org", "login") || !has("repo", "repository") {
			return fail("missing required argument: owner/repo")
		}
		owner := strArg("owner", "org", "login")
		name := strArg("repo", "repository")
		s.mu.RLock()
		repo := s.getRepo(owner, name)
		var j *RepoJSON
		if repo != nil {
			v := repo.json()
			j = &v
		}
		s.mu.RUnlock()
		if j == nil {
			return fail("Not Found")
		}
		return okResult(j)
	case match("create_issue") || match("issue_create"):
		if !has("owner", "org", "login") || !has("repo", "repository") || !has("title") {
			return fail("missing required argument")
		}
		owner := strArg("owner", "org", "login")
		name := strArg("repo", "repository")
		s.mu.Lock()
		repo := s.getRepo(owner, name)
		var iss *Issue
		if repo != nil {
			iss, _ = s.createIssue(repo, strArg("title"), strArg("body", "description"), nil)
		}
		s.mu.Unlock()
		if iss == nil {
			return fail("Not Found")
		}
		var j IssueJSON
		_ = json.Unmarshal(iss.Cached, &j)
		return okResult(j)
	case match("list_issues") || match("issues_list"):
		if !has("owner", "org", "login") || !has("repo", "repository") {
			return fail("missing required argument")
		}
		owner := strArg("owner", "org", "login")
		name := strArg("repo", "repository")
		state := strArg("state")
		if state == "" {
			state = "open"
		}
		s.mu.RLock()
		repo := s.getRepo(owner, name)
		var out []IssueJSON
		if repo != nil {
			for _, iss := range repo.Issues {
				if state == "all" || iss.State == state {
					var j IssueJSON
					_ = json.Unmarshal(iss.Cached, &j)
					out = append(out, j)
				}
			}
		}
		s.mu.RUnlock()
		if out == nil {
			out = []IssueJSON{}
		}
		return okResult(out)
	case match("create_pull_request") || match("create_pr"):
		if !has("owner", "org", "login") || !has("repo", "repository") || !has("title") || !has("head", "head_branch") || !has("base", "base_branch") {
			return fail("missing required argument")
		}
		owner := strArg("owner", "org", "login")
		name := strArg("repo", "repository")
		s.mu.Lock()
		repo := s.getRepo(owner, name)
		var pr *PR
		if repo != nil {
			pr, _ = s.createPR(repo, strArg("title"), strArg("body", "description"), strArg("head", "head_branch"), strArg("base", "base_branch"))
		}
		s.mu.Unlock()
		if pr == nil {
			return fail("could not create pull request")
		}
		s.mu.RLock()
		j := s.prJSON(pr)
		s.mu.RUnlock()
		return okResult(j)
	case match("merge_pull_request") || match("merge_pr"):
		if !has("owner", "org", "login") || !has("repo", "repository") || !has("pull_number", "number") {
			return fail("missing required argument")
		}
		owner := strArg("owner", "org", "login")
		name := strArg("repo", "repository")
		num := int(numArg("pull_number", "number"))
		s.mu.Lock()
		repo := s.getRepo(owner, name)
		var pr *PR
		if repo != nil {
			pr = repo.PRByNum[num]
		}
		if pr == nil {
			s.mu.Unlock()
			return fail("Not Found")
		}
		if s.prConflicted(pr) {
			s.mu.Unlock()
			return fail("merge conflict")
		}
		sha, ok := s.mergePR(pr)
		s.mu.Unlock()
		if !ok {
			return fail("could not merge pull request")
		}
		return okResult(map[string]any{"sha": sha, "merged": true, "message": "Pull request successfully merged"})
	default:
		return rpcError(req.ID, -32601, "Unknown tool: "+params.Name)
	}
}
