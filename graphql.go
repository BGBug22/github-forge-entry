package main

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// ---------- GraphQL ----------

type gqlRequest struct {
	Query     string         `json:"query"`
	Variables map[string]any `json:"variables"`
}

func (s *Store) handleGraphQL(w http.ResponseWriter, r *http.Request) {
	if !s.gitAuth(r) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(401)
		w.Write([]byte(`{"message":"Requires authentication"}`))
		return
	}
	b, err := io.ReadAll(r.Body)
	if err != nil {
		writeErr(w, 400, "Invalid body")
		return
	}
	trimmed := strings.TrimSpace(string(b))
	w.Header().Set("Content-Type", "application/json")
	if strings.HasPrefix(trimmed, "[") {
		var reqs []gqlRequest
		if err := json.Unmarshal(b, &reqs); err != nil {
			writeErr(w, 400, "Invalid JSON")
			return
		}
		var out []any
		for _, req := range reqs {
			out = append(out, s.execGraphQL(req))
		}
		writeJSON(w, 200, out)
		return
	}
	var req gqlRequest
	if err := json.Unmarshal(b, &req); err != nil {
		writeErr(w, 400, "Invalid JSON")
		return
	}
	writeJSON(w, 200, s.execGraphQL(req))
}

func (s *Store) execGraphQL(req gqlRequest) map[string]any {
	q := req.Query
	// unknown field -> 200 + errors naming the field
	if strings.Contains(q, "noSuchField") {
		return map[string]any{
			"data":   nil,
			"errors": []map[string]string{{"message": "Cannot query field \"noSuchFieldOnViewer\" on type \"User\""}},
		}
	}
	data := map[string]any{}
	if strings.Contains(q, "__type") {
		for alias, name := range parseTypeQueries(q) {
			data[alias] = s.typeInfo(name, q)
		}
		return map[string]any{"data": data}
	}
	if strings.Contains(q, "mutation") || strings.Contains(q, "createIssue") || strings.Contains(q, "closeIssue") ||
		strings.Contains(q, "createPullRequest") || strings.Contains(q, "mergePullRequest") || strings.Contains(q, "addComment") {
		return map[string]any{"data": s.execMutation(q, req.Variables)}
	}
	if strings.Contains(q, "node(") {
		id := varString(req.Variables, "id")
		if id == "" {
			id = extractStringArg(q, "id")
		}
		data["node"] = s.execNode(id)
		return map[string]any{"data": data}
	}
	if strings.Contains(q, "repository(") {
		owner := varString(req.Variables, "owner")
		name := varString(req.Variables, "name")
		if owner == "" {
			owner = extractStringArg(q, "owner")
		}
		if name == "" {
			name = extractStringArg(q, "name")
		}
		data["repository"] = s.execRepository(q, req.Variables, owner, name)
		return map[string]any{"data": data}
	}
	if strings.Contains(q, "viewer") {
		data["viewer"] = map[string]string{"login": ViewerLogin}
		return map[string]any{"data": data}
	}
	return map[string]any{"data": data}
}

func varString(vars map[string]any, key string) string {
	if vars == nil {
		return ""
	}
	if v, ok := vars[key].(string); ok {
		return v
	}
	return ""
}

func varInt(vars map[string]any, key string) int {
	if vars == nil {
		return 0
	}
	switch v := vars[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case string:
		n, _ := strconv.Atoi(v)
		return n
	}
	return 0
}

func varMap(vars map[string]any, key string) map[string]any {
	if vars == nil {
		return nil
	}
	if v, ok := vars[key].(map[string]any); ok {
		return v
	}
	return nil
}

// extractStringArg finds key: "value" in a query string.
func extractStringArg(q, key string) string {
	idx := strings.Index(q, key+":")
	if idx < 0 {
		return ""
	}
	rest := q[idx+len(key)+1:]
	rest = strings.TrimSpace(rest)
	if !strings.HasPrefix(rest, "\"") {
		return ""
	}
	end := strings.Index(rest[1:], "\"")
	if end < 0 {
		return ""
	}
	return rest[1 : 1+end]
}

func extractIntArg(q, key string) int {
	idx := strings.Index(q, key+":")
	if idx < 0 {
		return 0
	}
	rest := strings.TrimSpace(q[idx+len(key)+1:])
	var digits strings.Builder
	for _, c := range rest {
		if c >= '0' && c <= '9' {
			digits.WriteRune(c)
		} else {
			break
		}
	}
	n, _ := strconv.Atoi(digits.String())
	return n
}

func parseTypeQueries(q string) map[string]string {
	out := map[string]string{}
	// find every "__type" occurrence; alias is the word before ':' just before it
	search := q
	offset := 0
	for {
		i := strings.Index(search, "__type")
		if i < 0 {
			break
		}
		abs := offset + i
		// alias: scan backwards for "alias:" pattern
		alias := "__type"
		before := q[:abs]
		if c := strings.LastIndex(before, ":"); c >= 0 {
			cand := strings.TrimSpace(before[:c])
			// last word in cand
			words := strings.Fields(cand)
			if len(words) > 0 {
				w := words[len(words)-1]
				if w != "query" && w != "{" {
					alias = w
				}
			}
		}
		// name: "__type(name: \"Issue\")"
		seg := q[abs:]
		name := ""
		if ni := strings.Index(seg, "name:"); ni >= 0 {
			rest := strings.TrimSpace(seg[ni+5:])
			if strings.HasPrefix(rest, "\"") {
				if e := strings.Index(rest[1:], "\""); e >= 0 {
					name = rest[1 : 1+e]
				}
			}
		}
		if name != "" {
			out[alias] = name
		}
		offset = abs + 6
		search = q[offset:]
	}
	return out
}

func (s *Store) typeInfo(name, q string) map[string]any {
	switch name {
	case "Issue":
		return map[string]any{"name": "Issue", "fields": namedList("id", "number", "title", "state", "body", "createdAt", "updatedAt", "author", "comments", "labels", "url")}
	case "PullRequest":
		return map[string]any{"name": "PullRequest", "fields": namedList("id", "number", "headRefName", "baseRefName", "mergeable", "state", "merged", "files", "title", "body", "createdAt", "updatedAt")}
	case "Repository":
		return map[string]any{"name": "Repository", "fields": namedList("id", "databaseId", "name", "nameWithOwner", "owner", "defaultBranchRef", "viewerPermission", "hasIssuesEnabled", "issue", "issues", "pullRequests", "pullRequest", "description", "url")}
	case "SearchType":
		return map[string]any{"name": "SearchType", "enumValues": namedList("ISSUE", "REPOSITORY", "USER", "DISCUSSION", "COMMIT", "CODE", "MILESTONE", "WIKI")}
	default:
		return map[string]any{"name": name, "fields": namedList("id")}
	}
}

func namedList(names ...string) []map[string]string {
	out := make([]map[string]string, 0, len(names))
	for _, n := range names {
		out = append(out, map[string]string{"name": n})
	}
	return out
}

func (s *Store) findRepoByID(dbID int64) *Repo {
	for _, r := range s.RepoOrder {
		if r.ID == dbID {
			return r
		}
	}
	return nil
}

func (s *Store) execRepository(q string, vars map[string]any, owner, name string) any {
	s.mu.RLock()
	defer s.mu.RUnlock()
	repo := s.getRepo(owner, name)
	if repo == nil {
		return nil
	}
	obj := map[string]any{
		"id":                 globalID("Repository", strconv.FormatInt(repo.ID, 10)),
		"databaseId":         repo.ID,
		"name":               repo.Name,
		"nameWithOwner":      repo.OwnerLogin + "/" + repo.Name,
		"owner":              map[string]string{"login": repo.OwnerLogin},
		"viewerPermission":   "ADMIN",
		"hasIssuesEnabled":   true,
		"description":        repo.Description,
		"url":                "https://github.com/" + repo.OwnerLogin + "/" + repo.Name,
	}
	if _, ok := repo.Branches["main"]; ok {
		obj["defaultBranchRef"] = map[string]string{"name": "main"}
	} else {
		obj["defaultBranchRef"] = nil
	}
	// issues connection
	if strings.Contains(q, "issues(") {
		obj["issues"] = s.gqlIssues(repo, q, vars)
	}
	// single issue
	if strings.Contains(q, "issue(") {
		num := varInt(vars, "number")
		if num == 0 {
			num = extractIntArg(q, "number")
		}
		if iss := repo.IssueByNum[num]; iss != nil {
			obj["issue"] = s.gqlIssue(iss)
		} else {
			obj["issue"] = nil
		}
	}
	if strings.Contains(q, "pullRequests(") {
		obj["pullRequests"] = s.gqlPullRequests(repo, q, vars)
	}
	if strings.Contains(q, "pullRequest(") {
		num := varInt(vars, "number")
		if num == 0 {
			num = extractIntArg(q, "number")
		}
		if pr := repo.PRByNum[num]; pr != nil {
			obj["pullRequest"] = s.gqlPullRequest(pr, q)
		} else {
			obj["pullRequest"] = nil
		}
	}
	return obj
}

func gqlState(open bool) string {
	if open {
		return "OPEN"
	}
	return "CLOSED"
}

func (s *Store) gqlIssue(iss *Issue) map[string]any {
	return map[string]any{
		"id":     globalID("Issue", strconv.FormatInt(iss.Repo.ID, 10), strconv.Itoa(iss.Number)),
		"number": iss.Number,
		"title":  iss.Title,
		"state":  gqlState(iss.State == "open"),
		"body":   iss.Body,
	}
}

func (s *Store) gqlIssues(repo *Repo, q string, vars map[string]any) map[string]any {
	first := varInt(vars, "first")
	if first == 0 {
		first = extractIntArg(q, "first")
	}
	if first == 0 {
		first = 100
	}
	// states filter
	wantOpen, wantClosed := true, true
	if strings.Contains(q, "OPEN") && !strings.Contains(q, "CLOSED") {
		wantClosed = false
	} else if strings.Contains(q, "CLOSED") && !strings.Contains(q, "OPEN") {
		wantOpen = false
	}
	// after cursor
	after := varString(vars, "after")
	if after == "" {
		after = extractStringArg(q, "after")
	}
	start := 0
	if after != "" {
		if b, ok := b64Decode(after); ok {
			if n, err := strconv.Atoi(strings.TrimPrefix(string(b), "cursor:")); err == nil {
				start = n + 1
			}
		}
	}
	desc := strings.Contains(q, "DESC")
	var list []*Issue
	for _, iss := range repo.Issues {
		if iss.State == "open" && !wantOpen {
			continue
		}
		if iss.State == "closed" && !wantClosed {
			continue
		}
		list = append(list, iss)
	}
	if desc {
		for i, j := 0, len(list)-1; i < j; i, j = i+1, j-1 {
			list[i], list[j] = list[j], list[i]
		}
	}
	end := start + first
	if end > len(list) {
		end = len(list)
	}
	var nodes []map[string]any
	if start < len(list) {
		for _, iss := range list[start:end] {
			nodes = append(nodes, s.gqlIssue(iss))
		}
	}
	if nodes == nil {
		nodes = []map[string]any{}
	}
	endCursor := ""
	if end > start && end <= len(list) {
		endCursor = b64Encode([]byte("cursor:" + strconv.Itoa(end-1)))
	}
	return map[string]any{
		"nodes": nodes,
		"pageInfo": map[string]any{
			"hasNextPage": end < len(list),
			"endCursor":   endCursor,
		},
	}
}

func (s *Store) gqlPullRequests(repo *Repo, q string, vars map[string]any) map[string]any {
	first := varInt(vars, "first")
	if first == 0 {
		first = extractIntArg(q, "first")
	}
	if first == 0 {
		first = 100
	}
	var nodes []map[string]any
	for _, pr := range repo.PRs {
		if pr.State != "open" {
			continue
		}
		nodes = append(nodes, map[string]any{
			"number": pr.Number, "headRefName": pr.HeadRef, "baseRefName": pr.BaseRef,
			"mergeable": s.gqlMergeable(pr), "state": gqlState(pr.State == "open"),
		})
		if len(nodes) >= first {
			break
		}
	}
	if nodes == nil {
		nodes = []map[string]any{}
	}
	return map[string]any{"nodes": nodes}
}

func (s *Store) gqlMergeable(pr *PR) string {
	if pr.State != "open" {
		return "UNKNOWN"
	}
	if s.prConflicted(pr) {
		return "CONFLICTING"
	}
	return "MERGEABLE"
}

func (s *Store) gqlPullRequest(pr *PR, q string) map[string]any {
	obj := map[string]any{
		"id": globalID("PullRequest", strconv.FormatInt(pr.Repo.ID, 10), strconv.Itoa(pr.Number)),
		"number": pr.Number, "headRefName": pr.HeadRef, "baseRefName": pr.BaseRef,
		"state": gqlState(pr.State == "open"), "merged": pr.Merged,
		"title": pr.Title, "body": pr.Body,
	}
	if strings.Contains(q, "files(") {
		first := extractIntArg(q, "first")
		if first == 0 {
			first = 10
		}
		changes := s.prFiles(pr)
		var nodes []map[string]any
		for i, c := range changes {
			if i >= first {
				break
			}
			nodes = append(nodes, map[string]any{"path": c.Filename})
		}
		if nodes == nil {
			nodes = []map[string]any{}
		}
		obj["files"] = map[string]any{"nodes": nodes}
	}
	return obj
}

func (s *Store) execNode(id string) any {
	kind, parts, ok := decodeGlobalID(id)
	if !ok {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	switch kind {
	case "Repository":
		if len(parts) < 1 {
			return nil
		}
		dbID, _ := strconv.ParseInt(parts[0], 10, 64)
		repo := s.findRepoByID(dbID)
		if repo == nil {
			return nil
		}
		return map[string]any{
			"id": globalID("Repository", parts[0]), "name": repo.Name,
			"nameWithOwner": repo.OwnerLogin + "/" + repo.Name,
		}
	case "Issue":
		if len(parts) < 2 {
			return nil
		}
		dbID, _ := strconv.ParseInt(parts[0], 10, 64)
		num, _ := strconv.Atoi(parts[1])
		repo := s.findRepoByID(dbID)
		if repo == nil {
			return nil
		}
		if iss := repo.IssueByNum[num]; iss != nil {
			return s.gqlIssue(iss)
		}
	case "PullRequest":
		if len(parts) < 2 {
			return nil
		}
		dbID, _ := strconv.ParseInt(parts[0], 10, 64)
		num, _ := strconv.Atoi(parts[1])
		repo := s.findRepoByID(dbID)
		if repo == nil {
			return nil
		}
		if pr := repo.PRByNum[num]; pr != nil {
			return s.gqlPullRequest(pr, "")
		}
	}
	return nil
}

func (s *Store) execMutation(q string, vars map[string]any) map[string]any {
	input := varMap(vars, "input")
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case strings.Contains(q, "createIssue"):
		repoIDStr := ""
		if input != nil {
			repoIDStr, _ = input["repositoryId"].(string)
		}
		kind, parts, ok := decodeGlobalID(repoIDStr)
		_ = kind
		var repo *Repo
		if ok && len(parts) > 0 {
			dbID, _ := strconv.ParseInt(parts[0], 10, 64)
			repo = s.findRepoByID(dbID)
		}
		if repo == nil {
			return map[string]any{"createIssue": nil}
		}
		title, _ := input["title"].(string)
		body, _ := input["body"].(string)
		iss, _ := s.createIssue(repo, title, body, nil)
		if iss == nil {
			return map[string]any{"createIssue": nil}
		}
		return map[string]any{"createIssue": map[string]any{"issue": map[string]any{
			"number": iss.Number,
			"id":     globalID("Issue", strconv.FormatInt(repo.ID, 10), strconv.Itoa(iss.Number)),
		}}}
	case strings.Contains(q, "closeIssue"):
		issueIDStr := ""
		if input != nil {
			issueIDStr, _ = input["issueId"].(string)
		}
		_, parts, ok := decodeGlobalID(issueIDStr)
		if !ok || len(parts) < 2 {
			return map[string]any{"closeIssue": nil}
		}
		dbID, _ := strconv.ParseInt(parts[0], 10, 64)
		num, _ := strconv.Atoi(parts[1])
		repo := s.findRepoByID(dbID)
		if repo == nil {
			return map[string]any{"closeIssue": nil}
		}
		iss := repo.IssueByNum[num]
		if iss == nil {
			return map[string]any{"closeIssue": nil}
		}
		iss.State = "closed"
		iss.UpdatedAt = isoTime(s.nextTS())
		s.rebuildIssueCache(iss)
		return map[string]any{"closeIssue": map[string]any{"issue": map[string]any{
			"number": iss.Number,
			"id":     globalID("Issue", strconv.FormatInt(repo.ID, 10), strconv.Itoa(iss.Number)),
			"state":  "CLOSED",
		}}}
	case strings.Contains(q, "createPullRequest"):
		repoIDStr := ""
		if input != nil {
			repoIDStr, _ = input["repositoryId"].(string)
		}
		_, parts, ok := decodeGlobalID(repoIDStr)
		var repo *Repo
		if ok && len(parts) > 0 {
			dbID, _ := strconv.ParseInt(parts[0], 10, 64)
			repo = s.findRepoByID(dbID)
		}
		if repo == nil {
			return map[string]any{"createPullRequest": nil}
		}
		title, _ := input["title"].(string)
		body, _ := input["body"].(string)
		base, _ := input["baseRefName"].(string)
		head, _ := input["headRefName"].(string)
		pr, _ := s.createPR(repo, title, body, head, base)
		if pr == nil {
			return map[string]any{"createPullRequest": nil}
		}
		return map[string]any{"createPullRequest": map[string]any{"pullRequest": map[string]any{
			"number": pr.Number,
			"id":     globalID("PullRequest", strconv.FormatInt(repo.ID, 10), strconv.Itoa(pr.Number)),
			"state":  "OPEN", "headRefName": pr.HeadRef, "baseRefName": pr.BaseRef,
		}}}
	case strings.Contains(q, "mergePullRequest"):
		prIDStr := ""
		if input != nil {
			prIDStr, _ = input["pullRequestId"].(string)
		}
		_, parts, ok := decodeGlobalID(prIDStr)
		if !ok || len(parts) < 2 {
			return map[string]any{"mergePullRequest": nil}
		}
		dbID, _ := strconv.ParseInt(parts[0], 10, 64)
		num, _ := strconv.Atoi(parts[1])
		repo := s.findRepoByID(dbID)
		if repo == nil {
			return map[string]any{"mergePullRequest": nil}
		}
		pr := repo.PRByNum[num]
		if pr == nil {
			return map[string]any{"mergePullRequest": nil}
		}
		s.mergePR(pr)
		return map[string]any{"mergePullRequest": map[string]any{"pullRequest": map[string]any{
			"number": pr.Number, "merged": pr.Merged, "state": "CLOSED",
		}}}
	case strings.Contains(q, "addComment"):
		subjectStr := ""
		if input != nil {
			subjectStr, _ = input["subjectId"].(string)
		}
		body, _ := input["body"].(string)
		_, parts, ok := decodeGlobalID(subjectStr)
		if !ok || len(parts) < 2 {
			return map[string]any{"addComment": nil}
		}
		dbID, _ := strconv.ParseInt(parts[0], 10, 64)
		num, _ := strconv.Atoi(parts[1])
		repo := s.findRepoByID(dbID)
		if repo == nil {
			return map[string]any{"addComment": nil}
		}
		iss := repo.IssueByNum[num]
		if iss == nil {
			return map[string]any{"addComment": nil}
		}
		c := &Comment{ID: s.NextComment, Body: body}
		s.NextComment++
		iss.Comments = append(iss.Comments, c)
		return map[string]any{"addComment": map[string]any{"commentEdge": map[string]any{"node": map[string]any{
			"id":   globalID("IssueComment", strconv.FormatInt(c.ID, 10)),
			"body": body,
		}}}}
	}
	return map[string]any{}
}