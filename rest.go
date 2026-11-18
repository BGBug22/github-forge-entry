package main

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

func writeJSON(w http.ResponseWriter, status int, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		w.WriteHeader(500)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	w.Write(b)
}

func writeRawJSON(w http.ResponseWriter, status int, b []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	w.Write(b)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"message": msg})
}

func readJSONBody(r *http.Request, dst any) bool {
	if r.Body == nil {
		return false
	}
	b, err := io.ReadAll(r.Body)
	if err != nil {
		return false
	}
	if len(bytesTrimSpace(b)) == 0 {
		return true // empty body: leave dst zero
	}
	if err := json.Unmarshal(b, dst); err != nil {
		return false
	}
	return true
}

func bytesTrimSpace(b []byte) []byte {
	return []byte(strings.TrimSpace(string(b)))
}

func parseIntParam(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil || n <= 0 {
		return def
	}
	return n
}

// ---------- REST dispatch ----------

func (s *Store) handleREST(w http.ResponseWriter, r *http.Request) {
	// auth (healthz is handled elsewhere)
	if !s.gitAuth(r) {
		writeErr(w, 401, "Requires authentication")
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/api/v3")
	if path == "" || path == "/" {
		// root: 200 JSON + X-OAuth-Scopes (checked by the suite setup)
	if r.Method == "GET" {
			w.Header().Set("X-OAuth-Scopes", "repo, read:org")
			writeJSON(w, 200, map[string]string{"current_user_url": "http://" + r.Host + "/api/v3/user"})
			return
		}
		writeErr(w, 404, "Not Found")
		return
	}
	parts := strings.Split(strings.Trim(path, "/"), "/")
	switch {
	case len(parts) == 1 && parts[0] == "meta" && r.Method == "GET":
		writeJSON(w, 200, map[string]string{"installed_version": "1.0.0"})
	case len(parts) == 1 && parts[0] == "user" && r.Method == "GET":
		writeJSON(w, 200, map[string]any{"login": ViewerLogin, "id": ViewerID, "name": ViewerName, "email": ViewerEmail, "type": "User"})
	case len(parts) == 2 && parts[0] == "user" && parts[1] == "repos":
		if r.Method == "POST" {
			s.restCreateRepo(w, r)
		} else if r.Method == "GET" {
			s.restListUserRepos(w, r)
		} else {
			writeErr(w, 404, "Not Found")
		}
	case len(parts) >= 3 && parts[0] == "repos":
		s.restRepo(w, r, parts[1:])
	default:
		writeErr(w, 404, "Not Found")
	}
}

func (s *Store) restCreateRepo(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name        string `json:"name"`
		Private     bool   `json:"private"`
		Description string `json:"description"`
	}
	if !readJSONBody(r, &body) {
		writeErr(w, 400, "Invalid JSON")
		return
	}
	if body.Name == "" {
		writeErr(w, 422, "name is required")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	repo, ok := s.createRepo(ViewerLogin, ViewerID, body.Name, body.Private, body.Description)
	if !ok {
		writeErr(w, 422, "Repository already exists: "+body.Name)
		return
	}
	writeJSON(w, 201, repo.json())
}

func (s *Store) restListUserRepos(w http.ResponseWriter, r *http.Request) {
	perPage := parseIntParam(r.URL.Query().Get("per_page"), 30)
	page := parseIntParam(r.URL.Query().Get("page"), 1)
	s.mu.RLock()
	var mine []*Repo
	for _, repo := range s.RepoOrder {
		if repo.OwnerLogin == ViewerLogin {
			mine = append(mine, repo)
		}
	}
	total := len(mine)
	start := (page - 1) * perPage
	var out []RepoJSON
	if start < total {
		end := start + perPage
		if end > total {
			end = total
		}
		for _, repo := range mine[start:end] {
			out = append(out, repo.json())
		}
	}
	if out == nil {
		out = []RepoJSON{}
	}
	hasNext := start+perPage < total
	s.mu.RUnlock()
	if hasNext {
		nextURL := "http://" + r.Host + "/api/v3/user/repos?page=" + strconv.Itoa(page+1) + "&per_page=" + strconv.Itoa(perPage)
		w.Header().Set("Link", "<"+nextURL+">; rel=\"next\"")
	}
	writeJSON(w, 200, out)
}

func (s *Store) restRepo(w http.ResponseWriter, r *http.Request, parts []string) {
	// parts[0]=owner, parts[1]=repo, rest...
	if len(parts) < 2 {
		writeErr(w, 404, "Not Found")
		return
	}
	owner, name := parts[0], parts[1]
	s.mu.RLock()
	repo := s.getRepo(owner, name)
	s.mu.RUnlock()
	if repo == nil || repo.OwnerLogin != ViewerLogin {
		// owner != viewer -> 404 (also covers unknown repo)
		writeErr(w, 404, "Not Found")
		return
	}
	rest := parts[2:]
	if len(rest) == 0 {
		switch r.Method {
		case "GET":
			s.mu.RLock()
			j := repo.json()
			s.mu.RUnlock()
			writeJSON(w, 200, j)
		case "PATCH":
			var body struct {
				Description *string `json:"description"`
			}
			if !readJSONBody(r, &body) {
				writeErr(w, 400, "Invalid JSON")
				return
			}
			s.mu.Lock()
			if body.Description != nil {
				repo.Description = *body.Description
			}
			repo.UpdatedAt = isoTime(s.nextTS())
			j := repo.json()
			s.mu.Unlock()
			writeJSON(w, 200, j)
		case "DELETE":
			s.mu.Lock()
			delete(s.Repos, repoKey(owner, name))
			for i, rp := range s.RepoOrder {
				if rp == repo {
					s.RepoOrder = append(s.RepoOrder[:i], s.RepoOrder[i+1:]...)
					break
				}
			}
			s.mu.Unlock()
			w.WriteHeader(204)
		default:
			writeErr(w, 404, "Not Found")
		}
		return
	}
	switch rest[0] {
	case "git":
		s.restGit(w, r, repo, rest[1:])
	case "branches":
		s.restBranches(w, r, repo, rest[1:])
	case "commits":
		if r.Method == "GET" && len(rest) == 1 {
			s.restCommits(w, r, repo)
		} else {
			writeErr(w, 404, "Not Found")
		}
	case "contents":
		s.restContents(w, r, repo, rest[1:])
	case "labels":
		if r.Method == "POST" && len(rest) == 1 {
			s.restCreateLabel(w, r, repo)
		} else {
			writeErr(w, 404, "Not Found")
		}
	case "issues":
		s.restIssues(w, r, repo, rest[1:])
	case "pulls":
		s.restPulls(w, r, repo, rest[1:])
	default:
		writeErr(w, 404, "Not Found")
	}
}

func (s *Store) restGit(w http.ResponseWriter, r *http.Request, repo *Repo, rest []string) {
	// rest: ["refs"] or ["refs","heads",branch] ; POST refs
	if len(rest) >= 1 && rest[0] == "refs" {
		if r.Method == "POST" && len(rest) == 1 {
			var body struct {
				Ref string `json:"ref"`
				SHA string `json:"sha"`
			}
			if !readJSONBody(r, &body) {
				writeErr(w, 400, "Invalid JSON")
				return
			}
			if !strings.HasPrefix(body.Ref, "refs/heads/") {
				writeErr(w, 422, "only refs/heads supported")
				return
			}
			branch := strings.TrimPrefix(body.Ref, "refs/heads/")
			oid, ok := oidFromHex(body.SHA)
			if !ok {
				writeErr(w, 422, "invalid sha")
				return
			}
			s.mu.Lock()
			defer s.mu.Unlock()
			if _, exists := repo.Branches[branch]; exists {
				writeErr(w, 422, "Reference already exists")
				return
			}
			if _, ok := s.Objects[oid]; !ok {
				writeErr(w, 422, "object does not exist")
				return
			}
			repo.Branches[branch] = oid
			repo.BranchOrder = append(repo.BranchOrder, branch)
			s.invalidate(repo)
			writeJSON(w, 201, map[string]any{"ref": body.Ref, "object": map[string]string{"sha": oid.Hex()}})
			return
		}
		if r.Method == "GET" && len(rest) == 3 && rest[1] == "heads" {
			branch := rest[2]
			s.mu.RLock()
			oid, ok := repo.Branches[branch]
			s.mu.RUnlock()
			if !ok {
				writeErr(w, 404, "Not Found")
				return
			}
			writeJSON(w, 200, map[string]any{"ref": "refs/heads/" + branch, "object": map[string]string{"sha": oid.Hex()}})
			return
		}
	}
	writeErr(w, 404, "Not Found")
}

func (s *Store) restBranches(w http.ResponseWriter, r *http.Request, repo *Repo, rest []string) {
	if r.Method != "GET" {
		writeErr(w, 404, "Not Found")
		return
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if len(rest) == 0 {
		branches, _ := s.allRefs(repo)
		out := make([]map[string]any, 0, len(branches))
		for _, b := range branches {
			out = append(out, map[string]any{"name": b, "commit": map[string]string{"sha": repo.Branches[b].Hex()}})
		}
		writeJSON(w, 200, out)
		return
	}
	if len(rest) == 1 {
		oid, ok := repo.Branches[rest[0]]
		if !ok {
			writeErr(w, 404, "Not Found")
			return
		}
		writeJSON(w, 200, map[string]any{"name": rest[0], "commit": map[string]string{"sha": oid.Hex()}})
		return
	}
	writeErr(w, 404, "Not Found")
}

func (s *Store) restCommits(w http.ResponseWriter, r *http.Request, repo *Repo) {
	branch := r.URL.Query().Get("sha")
	if branch == "" {
		branch = "main"
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	tip, ok := repo.Branches[branch]
	if !ok {
		// branch may be a commit sha directly
		if oid, ok2 := oidFromHex(branch); ok2 {
			tip = oid
			ok = true
		}
	}
	if !ok {
		writeJSON(w, 200, []any{})
		return
	}
	var out []map[string]any
	seen := make(map[OID]bool)
	cur := tip
	for !cur.IsZero() && !seen[cur] {
		seen[cur] = true
		ci := s.parseCommit(cur)
		if ci == nil {
			break
		}
		parents := make([]map[string]string, 0, len(ci.Parents))
		for _, p := range ci.Parents {
			parents = append(parents, map[string]string{"sha": p.Hex()})
		}
		out = append(out, map[string]any{
			"sha": cur.Hex(),
			"commit": map[string]any{
				"message": strings.TrimSuffix(ci.Message, "\n"),
				"author": map[string]string{
					"name": ci.Author, "email": ci.AuthorEM, "date": isoTime(ci.AuthorTS),
				},
				"committer": map[string]string{
					"name": ci.Committer, "email": ci.CommitEM, "date": isoTime(ci.CommitTS),
				},
			},
			"parents": parents,
		})
		if len(ci.Parents) == 0 {
			break
		}
		cur = ci.Parents[0]
	}
	if out == nil {
		out = []map[string]any{}
	}
	writeJSON(w, 200, out)
}

func (s *Store) restContents(w http.ResponseWriter, r *http.Request, repo *Repo, rest []string) {
	path := strings.Join(rest, "/")
	branch := r.URL.Query().Get("ref")
	if branch == "" {
		branch = "main"
	}
	switch r.Method {
	case "GET":
		s.mu.RLock()
		defer s.mu.RUnlock()
		tip, ok := repo.Branches[branch]
		if !ok {
			writeErr(w, 404, "Not Found")
			return
		}
		ci := s.parseCommit(tip)
		if ci == nil {
			writeErr(w, 404, "Not Found")
			return
		}
		files := s.flatFiles(ci.Tree)
		if blobOID, isFile := files[path]; isFile {
			content, _ := s.objectContent(blobOID)
			writeJSON(w, 200, map[string]any{
				"type": "file", "encoding": "base64",
				"content": base64.StdEncoding.EncodeToString(content),
				"sha": blobOID.Hex(), "size": len(content),
				"name": lastPathPart(path), "path": path,
			})
			return
		}
		// directory?
		dirOID, ok := s.dirTreeOID(repo, branch, path)
		if !ok && path != "" {
			writeErr(w, 404, "Not Found")
			return
		}
		if path == "" {
			dirOID = ci.Tree
		}
		entries := s.parseTree(dirOID)
		out := make([]map[string]any, 0, len(entries))
		for _, e := range entries {
			typ := "file"
			if e.Mode == "40000" {
				typ = "dir"
			}
			p := path
			if p != "" {
				p += "/"
			}
			p += e.Name
			out = append(out, map[string]any{"type": typ, "name": e.Name, "path": p, "sha": e.OID.Hex()})
		}
		sort.Slice(out, func(i, j int) bool { return out[i]["name"].(string) < out[j]["name"].(string) })
		writeJSON(w, 200, out)
	case "PUT":
		var body struct {
			Message string `json:"message"`
			Content string `json:"content"`
			SHA     string `json:"sha"`
			Branch  string `json:"branch"`
		}
		if !readJSONBody(r, &body) {
			writeErr(w, 400, "Invalid JSON")
			return
		}
		raw, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(strings.TrimSpace(body.Content), "\n", ""))
		if err != nil {
			writeErr(w, 400, "Invalid base64 content")
			return
		}
		br := body.Branch
		if br == "" {
			br = branch
		}
		s.mu.Lock()
		blob, commit, status, msg := s.putContent(repo, br, path, raw, body.Message, body.SHA)
		s.mu.Unlock()
		if status >= 400 {
			writeErr(w, status, msg)
			return
		}
		writeJSON(w, status, map[string]any{
			"content": map[string]any{"sha": blob.Hex(), "encoding": "base64", "name": lastPathPart(path), "path": path, "size": len(raw)},
			"commit":  map[string]any{"sha": commit.Hex(), "message": body.Message},
		})
	case "DELETE":
		var body struct {
			Message string `json:"message"`
			SHA     string `json:"sha"`
		}
		// body may be empty; ignore decode failure when empty
		if r.Body != nil {
			b, _ := io.ReadAll(r.Body)
			if len(bytesTrimSpace(b)) > 0 {
				_ = json.Unmarshal(b, &body)
			}
		}
		br := r.URL.Query().Get("ref")
		if br == "" {
			br = "main"
		}
		s.mu.Lock()
		commit, status, msg := s.deleteContent(repo, br, path, body.Message, body.SHA)
		s.mu.Unlock()
		if status >= 400 {
			writeErr(w, status, msg)
			return
		}
		writeJSON(w, 200, map[string]any{"commit": map[string]any{"sha": commit.Hex()}})
	default:
		writeErr(w, 404, "Not Found")
	}
}

func lastPathPart(p string) string {
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return p
}

func (s *Store) restCreateLabel(w http.ResponseWriter, r *http.Request, repo *Repo) {
	var body struct {
		Name  string `json:"name"`
		Color string `json:"color"`
	}
	if !readJSONBody(r, &body) {
		writeErr(w, 400, "Invalid JSON")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	repo.Labels = append(repo.Labels, Label{Name: body.Name, Color: body.Color})
	repo.LabelSet[body.Name] = true
	writeJSON(w, 201, map[string]string{"name": body.Name, "color": body.Color})
}

func (s *Store) restIssues(w http.ResponseWriter, r *http.Request, repo *Repo, rest []string) {
	if len(rest) == 0 {
		switch r.Method {
		case "POST":
			var body struct {
				Title  string   `json:"title"`
				Body   string   `json:"body"`
				Labels []string `json:"labels"`
			}
			if !readJSONBody(r, &body) {
				writeErr(w, 400, "Invalid JSON")
				return
			}
			s.mu.Lock()
			iss, errMsg := s.createIssue(repo, body.Title, body.Body, body.Labels)
			s.mu.Unlock()
			if iss == nil {
				writeErr(w, 422, errMsg)
				return
			}
			writeRawJSON(w, 201, iss.Cached)
		case "GET":
			s.restListIssues(w, r, repo)
		default:
			writeErr(w, 404, "Not Found")
		}
		return
	}
	// /issues/{n} or /issues/{n}/comments
	n, err := strconv.Atoi(rest[0])
	if err != nil {
		writeErr(w, 404, "Not Found")
		return
	}
	if len(rest) == 1 {
		switch r.Method {
		case "GET":
			s.mu.RLock()
			iss := repo.IssueByNum[n]
			s.mu.RUnlock()
			if iss == nil {
				writeErr(w, 404, "Not Found")
				return
			}
			writeRawJSON(w, 200, iss.Cached)
		case "PATCH":
			var body struct {
				State string `json:"state"`
				Title string `json:"title"`
				Body  string `json:"body"`
			}
			if !readJSONBody(r, &body) {
				writeErr(w, 400, "Invalid JSON")
				return
			}
			s.mu.Lock()
			iss := repo.IssueByNum[n]
			if iss == nil {
				s.mu.Unlock()
				writeErr(w, 404, "Not Found")
				return
			}
			if body.State != "" {
				iss.State = body.State
			}
			if body.Title != "" {
				iss.Title = body.Title
			}
			if body.Body != "" {
				iss.Body = body.Body
			}
			iss.UpdatedAt = isoTime(s.nextTS())
			s.rebuildIssueCache(iss)
			cached := iss.Cached
			s.mu.Unlock()
			writeRawJSON(w, 200, cached)
		default:
			writeErr(w, 404, "Not Found")
		}
		return
	}
	if len(rest) == 2 && rest[1] == "comments" {
		switch r.Method {
		case "POST":
			var body struct {
				Body string `json:"body"`
			}
			if !readJSONBody(r, &body) {
				writeErr(w, 400, "Invalid JSON")
				return
			}
			s.mu.Lock()
			iss := repo.IssueByNum[n]
			if iss == nil {
				s.mu.Unlock()
				writeErr(w, 404, "Not Found")
				return
			}
			c := &Comment{ID: s.NextComment, Body: body.Body}
			s.NextComment++
			iss.Comments = append(iss.Comments, c)
			s.mu.Unlock()
			writeJSON(w, 201, map[string]any{"id": c.ID, "body": c.Body})
		case "GET":
			s.mu.RLock()
			iss := repo.IssueByNum[n]
			s.mu.RUnlock()
			if iss == nil {
				writeErr(w, 404, "Not Found")
				return
			}
			out := make([]map[string]any, 0, len(iss.Comments))
			for _, c := range iss.Comments {
				out = append(out, map[string]any{"id": c.ID, "body": c.Body})
			}
			writeJSON(w, 200, out)
		default:
			writeErr(w, 404, "Not Found")
		}
		return
	}
	writeErr(w, 404, "Not Found")
}

func (s *Store) restListIssues(w http.ResponseWriter, r *http.Request, repo *Repo) {
	q := r.URL.Query()
	state := q.Get("state")
	if state == "" {
		state = "open"
	}
	sortBy := q.Get("sort")
	direction := q.Get("direction")
	if direction == "" {
		direction = "desc"
	}
	perPage := parseIntParam(q.Get("per_page"), 30)
	page := parseIntParam(q.Get("page"), 1)
	s.mu.RLock()
	var list []*Issue
	for _, iss := range repo.Issues {
		if state == "all" || iss.State == state {
			list = append(list, iss)
		}
	}
	s.mu.RUnlock()
	// sort=created: order by creation (number); asc = number up, desc = down
	if sortBy == "created" || sortBy == "" {
		if direction == "asc" {
			// list already in creation order
		} else {
			for i, j := 0, len(list)-1; i < j; i, j = i+1, j-1 {
				list[i], list[j] = list[j], list[i]
			}
		}
	}
	total := len(list)
	start := (page - 1) * perPage
	var raws []json.RawMessage
	if start < total {
		end := start + perPage
		if end > total {
			end = total
		}
		for _, iss := range list[start:end] {
			raws = append(raws, iss.Cached)
		}
	}
	if raws == nil {
		raws = []json.RawMessage{}
	}
	hasNext := start+perPage < total
	if hasNext {
		nextURL := "http://" + r.Host + r.URL.Path + "?state=" + state + "&page=" + strconv.Itoa(page+1) + "&per_page=" + strconv.Itoa(perPage)
		w.Header().Set("Link", "<"+nextURL+">; rel=\"next\"")
	}
	writeJSON(w, 200, raws)
}

func (s *Store) restPulls(w http.ResponseWriter, r *http.Request, repo *Repo, rest []string) {
	if len(rest) == 0 {
		switch r.Method {
		case "POST":
			var body struct {
				Title string `json:"title"`
				Body  string `json:"body"`
				Head  string `json:"head"`
				Base  string `json:"base"`
			}
			if !readJSONBody(r, &body) {
				writeErr(w, 400, "Invalid JSON")
				return
			}
			s.mu.Lock()
			pr, errMsg := s.createPR(repo, body.Title, body.Body, body.Head, body.Base)
			s.mu.Unlock()
			if pr == nil {
				writeErr(w, 422, errMsg)
				return
			}
			s.mu.RLock()
			j := s.prJSON(pr)
			s.mu.RUnlock()
			writeJSON(w, 201, j)
		case "GET":
			state := r.URL.Query().Get("state")
			if state == "" {
				state = "open"
			}
			s.mu.RLock()
			var out []PRJSON
			for _, pr := range repo.PRs {
				if state == "all" || pr.State == state {
					out = append(out, s.prJSON(pr))
				}
			}
			s.mu.RUnlock()
			if out == nil {
				out = []PRJSON{}
			}
			writeJSON(w, 200, out)
		default:
			writeErr(w, 404, "Not Found")
		}
		return
	}
	n, err := strconv.Atoi(rest[0])
	if err != nil {
		writeErr(w, 404, "Not Found")
		return
	}
	if len(rest) == 1 {
		if r.Method != "GET" {
			writeErr(w, 404, "Not Found")
			return
		}
		s.mu.RLock()
		pr := repo.PRByNum[n]
		var j *PRJSON
		if pr != nil {
			v := s.prJSON(pr)
			j = &v
		}
		s.mu.RUnlock()
		if j == nil {
			writeErr(w, 404, "Not Found")
			return
		}
		writeJSON(w, 200, j)
		return
	}
	if len(rest) == 2 && rest[1] == "files" && r.Method == "GET" {
		s.mu.RLock()
		pr := repo.PRByNum[n]
		var files []FileChange
		if pr != nil {
			files = s.prFiles(pr)
		}
		s.mu.RUnlock()
		if pr == nil {
			writeErr(w, 404, "Not Found")
			return
		}
		writeJSON(w, 200, files)
		return
	}
	if len(rest) == 2 && rest[1] == "merge" && r.Method == "PUT" {
		var body struct {
			MergeMethod string `json:"merge_method"`
		}
		// body optional; ignore decode errors on empty body
		if r.Body != nil {
			b, _ := io.ReadAll(r.Body)
			if len(bytesTrimSpace(b)) > 0 {
				_ = json.Unmarshal(b, &body)
			}
		}
		s.mu.Lock()
		pr := repo.PRByNum[n]
		if pr == nil {
			s.mu.Unlock()
			writeErr(w, 404, "Not Found")
			return
		}
		if s.prConflicted(pr) {
			s.mu.Unlock()
			writeErr(w, 405, "Merge conflict")
			return
		}
		sha, ok := s.mergePR(pr)
		s.mu.Unlock()
		if !ok {
			writeErr(w, 404, "Not Found")
			return
		}
		writeJSON(w, 200, map[string]any{"sha": sha, "merged": true, "message": "Pull request successfully merged"})
		return
	}
	writeErr(w, 404, "Not Found")
}
