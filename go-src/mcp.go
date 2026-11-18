// MCP: JSON-RPC 2.0 Streamable HTTP, revisi 2026-07-28. Mendukung
// server/discover (modern), initialize + notifications/initialized (legacy),
// tools/list (pre-serialized, byte-identik), tools/call ke 6 tool kanonis
// atas store yang sama dengan REST.
package main

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"sync"
)

type mcpTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

var mcpTools = []mcpTool{
	{
		Name:        "create_repo",
		Description: "Create a new repository owned by the viewer.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"name":        map[string]any{"type": "string"},
				"description": map[string]any{"type": "string"},
				"private":     map[string]any{"type": "boolean"},
				"owner":       map[string]any{"type": "string"},
				"full_name":   map[string]any{"type": "string"},
			},
			"required": []string{"name"},
		},
	},
	{
		Name:        "get_repo",
		Description: "Get a repository by owner and name.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"owner":     map[string]any{"type": "string"},
				"repo":      map[string]any{"type": "string"},
				"full_name": map[string]any{"type": "string"},
			},
			"required": []string{"owner", "repo"},
		},
	},
	{
		Name:        "create_issue",
		Description: "Create an issue in a repository.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"owner":       map[string]any{"type": "string"},
				"repo":        map[string]any{"type": "string"},
				"full_name":   map[string]any{"type": "string"},
				"title":       map[string]any{"type": "string"},
				"body":        map[string]any{"type": "string"},
				"description": map[string]any{"type": "string"},
			},
			"required": []string{"owner", "repo", "title"},
		},
	},
	{
		Name:        "list_issues",
		Description: "List issues in a repository.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"owner":     map[string]any{"type": "string"},
				"repo":      map[string]any{"type": "string"},
				"full_name": map[string]any{"type": "string"},
				"state":     map[string]any{"type": "string"},
			},
			"required": []string{"owner", "repo"},
		},
	},
	{
		Name:        "create_pull_request",
		Description: "Create a pull request from a head branch into a base branch.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"owner":       map[string]any{"type": "string"},
				"repo":        map[string]any{"type": "string"},
				"full_name":   map[string]any{"type": "string"},
				"title":       map[string]any{"type": "string"},
				"head":        map[string]any{"type": "string"},
				"head_branch": map[string]any{"type": "string"},
				"base":        map[string]any{"type": "string"},
				"base_branch": map[string]any{"type": "string"},
				"body":        map[string]any{"type": "string"},
				"description": map[string]any{"type": "string"},
			},
			"required": []string{"owner", "repo", "title", "head", "base"},
		},
	},
	{
		Name:        "merge_pull_request",
		Description: "Merge a pull request by number (squash merge).",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"owner":       map[string]any{"type": "string"},
				"repo":        map[string]any{"type": "string"},
				"full_name":   map[string]any{"type": "string"},
				"pull_number": map[string]any{"type": "integer"},
				"number":      map[string]any{"type": "integer"},
			},
			"required": []string{"owner", "repo", "pull_number"},
		},
	},
}

// toolsListResult di-serialisasi SEKALI agar dua panggilan byte-identik.
var mcpToolsListJSON []byte
var mcpToolsOnce sync.Once

func toolsListJSON() []byte {
	mcpToolsOnce.Do(func() {
		mcpToolsListJSON, _ = json.Marshal(map[string]any{"tools": mcpTools})
	})
	return mcpToolsListJSON
}

func handleMCP(w http.ResponseWriter, r *http.Request) {
	// Echo session id bila klien mengirimkannya.
	if sid := r.Header.Get("Mcp-Session-Id"); sid != "" {
		w.Header().Set("Mcp-Session-Id", sid)
	}
	body, ok := readBody(r)
	if !ok {
		mcpError(w, nil, -32700, "Parse error")
		return
	}
	var req map[string]any
	if err := json.Unmarshal(body, &req); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(400)
		_, _ = w.Write(jsonBytes(map[string]any{
			"jsonrpc": "2.0", "id": nil,
			"error": map[string]any{"code": -32700, "message": "Parse error"},
		}))
		return
	}
	method, _ := req["method"].(string)
	id, hasID := req["id"]
	if !hasID || id == nil {
		// Notifikasi: notifications/initialized -> 202 kosong.
		w.WriteHeader(202)
		return
	}
	switch method {
	case "server/discover":
		mcpResult(w, id, map[string]any{
			"protocolVersion":   "2026-07-28",
			"supportedVersions": []string{"2026-07-28", "2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"},
			"capabilities":      map[string]any{"tools": map[string]any{}},
			"serverInfo":        map[string]any{"name": "forge-go", "version": "1.0.0"},
			"_meta": map[string]any{
				"io.modelcontextprotocol/serverInfo": map[string]any{"name": "forge-go", "version": "1.0.0"},
			},
		})
	case "initialize":
		// Pilih protocolVersion dari yang ditawarkan klien.
		chosen := "2025-11-25"
		if params, ok := req["params"].(map[string]any); ok {
			if pv, ok := params["protocolVersion"].(string); ok && pv != "" {
				chosen = pv
			}
		}
		w.Header().Set("Mcp-Session-Id", "mcp-session-1")
		mcpResult(w, id, map[string]any{
			"protocolVersion": chosen,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "forge-go", "version": "1.0.0"},
		})
	case "ping":
		mcpResult(w, id, map[string]any{})
	case "tools/list":
		// Byte-identik: kirim hasil pre-serialisasi langsung.
		w.Header().Set("Content-Type", "application/json")
		envelope := map[string]any{
			"jsonrpc": "2.0", "id": id,
			"result": json.RawMessage(toolsListJSON()),
		}
		w.WriteHeader(200)
		_, _ = w.Write(jsonBytes(envelope))
	case "tools/call":
		mcpToolsCall(w, id, req)
	default:
		mcpError(w, id, -32601, "Method not found: "+method)
	}
}

func mcpResult(w http.ResponseWriter, id any, result map[string]any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(200)
	_, _ = w.Write(jsonBytes(map[string]any{"jsonrpc": "2.0", "id": id, "result": result}))
}

func mcpError(w http.ResponseWriter, id any, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(200)
	_, _ = w.Write(jsonBytes(map[string]any{
		"jsonrpc": "2.0", "id": id,
		"error": map[string]any{"code": code, "message": msg},
	}))
}

func mcpTextResult(text string, structured any) map[string]any {
	return map[string]any{
		"content":           []any{map[string]any{"type": "text", "text": text}},
		"structuredContent": structured,
	}
}

func mcpIsError(text string) map[string]any {
	return map[string]any{
		"isError": true,
		"content": []any{map[string]any{"type": "text", "text": text}},
	}
}

// mcpArg mengambil argumen dengan sinonim nama properti.
func mcpArg(args map[string]any, names ...string) string {
	for _, n := range names {
		if v, ok := args[n]; ok {
			if s, ok := v.(string); ok && s != "" {
				return s
			}
		}
	}
	return ""
}

func mcpArgInt(args map[string]any, names ...string) (int, bool) {
	for _, n := range names {
		if v, ok := args[n]; ok {
			switch x := v.(type) {
			case float64:
				return int(x), true
			case int:
				return x, true
			case string:
				if i, err := strconv.Atoi(x); err == nil {
					return i, true
				}
			}
		}
	}
	return 0, false
}

// mcpOwnerRepo mengurai owner/repo dari argumen atau full_name.
func mcpOwnerRepo(args map[string]any) (string, string) {
	owner := mcpArg(args, "owner", "org", "login")
	repo := mcpArg(args, "repo", "repository")
	if fn := mcpArg(args, "full_name"); fn != "" {
		parts := strings.SplitN(fn, "/", 2)
		if len(parts) == 2 {
			if owner == "" {
				owner = parts[0]
			}
			if repo == "" {
				repo = parts[1]
			}
		}
	}
	return owner, repo
}

func mcpToolsCall(w http.ResponseWriter, id any, req map[string]any) {
	params, _ := req["params"].(map[string]any)
	name, _ := params["name"].(string)
	args, _ := params["arguments"].(map[string]any)
	if args == nil {
		args = map[string]any{}
	}
	// Cocokkan nama tool sebagai suffix, case-insensitive (prefix github__ OK).
	toolName := ""
	for _, t := range mcpTools {
		if strings.EqualFold(name, t.Name) || strings.HasSuffix(strings.ToLower(name), strings.ToLower(t.Name)) {
			toolName = t.Name
			break
		}
	}
	if toolName == "" {
		mcpError(w, id, -32601, "Unknown tool: "+name)
		return
	}
	result, isErr := executeMCPTool(toolName, args)
	if isErr {
		txt, _ := result["text"].(string)
		if txt == "" {
			txt = "tool error"
		}
		mcpResult(w, id, mcpIsError(txt))
		return
	}
	mcpResult(w, id, result)
}

func executeMCPTool(name string, args map[string]any) (map[string]any, bool) {
	switch name {
	case "create_repo":
		repoName := mcpArg(args, "name")
		if repoName == "" {
			return map[string]any{"text": "missing required argument: name"}, true
		}
		st.mu.Lock()
		if _, exists := st.repos[viewerLogin+"/"+repoName]; exists {
			st.mu.Unlock()
			return map[string]any{"text": "repository already exists"}, true
		}
		seq := st.nextSeq()
		rp := &repo{
			ID: st.nextRepo, Owner: viewerLogin, Name: repoName,
			Description:   mcpArg(args, "description"),
			Private:       false,
			DefaultBranch: "main", CreatedSeq: seq,
			Refs: map[string]oid{}, Objects: map[oid]*gitObject{},
			IssuesByNum: map[int]*issue{}, PullsByNum: map[int]*pull{},
			NextIssue: 1, NextPull: 1,
			Labels: map[string]*label{}, Comments: map[int][]*comment{},
			packCache: map[string][]byte{},
		}
		if v, ok := args["private"]; ok {
			if b, ok := v.(bool); ok {
				rp.Private = b
			}
		}
		st.nextRepo++
		st.repos[viewerLogin+"/"+repoName] = rp
		st.order = append(st.order, rp)
		out := st.repoToJSON(rp)
		st.mu.Unlock()
		return mcpTextResult(string(jsonBytes(out)), out), false
	case "get_repo":
		owner, repoName := mcpOwnerRepo(args)
		if owner == "" || repoName == "" {
			return map[string]any{"text": "missing required argument: owner or repo"}, true
		}
		st.mu.RLock()
		rp := st.getRepo(owner, repoName)
		if rp == nil {
			st.mu.RUnlock()
			return map[string]any{"text": "Not Found"}, true
		}
		out := st.repoToJSON(rp)
		st.mu.RUnlock()
		return mcpTextResult(string(jsonBytes(out)), out), false
	case "create_issue":
		owner, repoName := mcpOwnerRepo(args)
		title := mcpArg(args, "title")
		if owner == "" || repoName == "" || title == "" {
			return map[string]any{"text": "missing required argument"}, true
		}
		st.mu.Lock()
		rp := st.getRepo(owner, repoName)
		if rp == nil {
			st.mu.Unlock()
			return map[string]any{"text": "Not Found"}, true
		}
		seq := st.nextSeq()
		num := rp.NextIssue
		rp.NextIssue++
		st.nextID++
		is := &issue{
			ID: st.nextID, Number: num, Title: title,
			Body:  mcpArg(args, "body", "description"),
			State: "open", CreatedAt: isoTime(seq), UpdatedAt: isoTime(seq), Seq: seq,
		}
		rp.Issues = append(rp.Issues, is)
		rp.IssuesByNum[num] = is
		st.rebuildIssueCache(rp, is)
		cached := is.cached
		st.mu.Unlock()
		return mcpTextResult(string(cached), json.RawMessage(cached)), false
	case "list_issues":
		owner, repoName := mcpOwnerRepo(args)
		if owner == "" || repoName == "" {
			return map[string]any{"text": "missing required argument: owner or repo"}, true
		}
		state := mcpArg(args, "state")
		if state == "" {
			state = "open"
		}
		st.mu.RLock()
		rp := st.getRepo(owner, repoName)
		if rp == nil {
			st.mu.RUnlock()
			return map[string]any{"text": "Not Found"}, true
		}
		list := []json.RawMessage{}
		for _, is := range rp.Issues {
			if state != "all" && is.State != state {
				continue
			}
			list = append(list, is.cached)
		}
		st.mu.RUnlock()
		text := string(jsonBytes(list))
		return mcpTextResult(text, json.RawMessage(text)), false
	case "create_pull_request":
		owner, repoName := mcpOwnerRepo(args)
		title := mcpArg(args, "title")
		head := normalizeRef(mcpArg(args, "head", "head_branch"))
		base := normalizeRef(mcpArg(args, "base", "base_branch"))
		if owner == "" || repoName == "" || title == "" || head == "" || base == "" {
			return map[string]any{"text": "missing required argument"}, true
		}
		st.mu.Lock()
		rp := st.getRepo(owner, repoName)
		if rp == nil {
			st.mu.Unlock()
			return map[string]any{"text": "Not Found"}, true
		}
		headOID, headOK := rp.refTip("refs/heads/" + head)
		baseOID, baseOK := rp.refTip("refs/heads/" + base)
		if !headOK || !baseOK {
			st.mu.Unlock()
			return map[string]any{"text": "head or base branch does not exist"}, true
		}
		for _, p := range rp.Pulls {
			if p.State == "open" && p.HeadRef == head && p.BaseRef == base {
				out := st.pullToJSON(rp, p)
				st.mu.Unlock()
				return mcpTextResult(string(jsonBytes(out)), out), false
			}
		}
		seq := st.nextSeq()
		num := rp.NextPull
		rp.NextPull++
		st.nextID++
		p := &pull{
			ID: st.nextID, Number: num, Title: title,
			Body:  mcpArg(args, "body", "description"),
			State: "open", HeadRef: head, BaseRef: base,
			HeadSHA: headOID.hex(), BaseSHA: baseOID.hex(),
			CreatedAt: isoTime(seq), UpdatedAt: isoTime(seq), Seq: seq,
		}
		p.Files = st.computePullFiles(rp, p)
		rp.Pulls = append(rp.Pulls, p)
		rp.PullsByNum[num] = p
		out := st.pullToJSON(rp, p)
		st.mu.Unlock()
		return mcpTextResult(string(jsonBytes(out)), out), false
	case "merge_pull_request":
		owner, repoName := mcpOwnerRepo(args)
		num, ok := mcpArgInt(args, "pull_number", "number")
		if owner == "" || repoName == "" || !ok {
			return map[string]any{"text": "missing required argument: pull_number"}, true
		}
		st.mu.Lock()
		rp := st.getRepo(owner, repoName)
		if rp == nil {
			st.mu.Unlock()
			return map[string]any{"text": "Not Found"}, true
		}
		p, exists := rp.PullsByNum[num]
		if !exists {
			st.mu.Unlock()
			return map[string]any{"text": "Not Found"}, true
		}
		mergeID, status, msg := st.doMergePull(rp, p)
		st.mu.Unlock()
		if status == 405 {
			return map[string]any{"text": "Merge conflict"}, true
		}
		out := map[string]any{"sha": mergeID.hex(), "merged": true, "message": msg}
		return mcpTextResult(string(jsonBytes(out)), out), false
	}
	return map[string]any{"text": "Unknown tool"}, true
}
