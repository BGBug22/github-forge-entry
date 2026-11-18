// REST v3: repositories, contents, branches, commits, issues, labels,
// comments, pull requests. Semua jawaban application/json dengan
// X-GitHub-Request-Id; error membawa {"message": string}.
package main

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// Bentuk JSON
// ---------------------------------------------------------------------------

type userJSON struct {
	Login     string `json:"login"`
	ID        int    `json:"id"`
	AvatarURL string `json:"avatar_url"`
	Type      string `json:"type"`
	HTMLURL   string `json:"html_url"`
	URL       string `json:"url"`
}

func viewerUserJSON() userJSON {
	return userJSON{Login: viewerLogin, ID: 1000, AvatarURL: "https://example.invalid/avatar/" + viewerLogin, Type: "User", HTMLURL: "https://example.invalid/" + viewerLogin, URL: "/api/v3/users/" + viewerLogin}
}

type repoJSON struct {
	ID               int      `json:"id"`
	NodeID           string   `json:"node_id"`
	Name             string   `json:"name"`
	FullName         string   `json:"full_name"`
	Owner            userJSON `json:"owner"`
	Private          bool     `json:"private"`
	Description      string   `json:"description"`
	DefaultBranch    string   `json:"default_branch"`
	CreatedAt        string   `json:"created_at"`
	UpdatedAt        string   `json:"updated_at"`
	PushedAt         string   `json:"pushed_at"`
	HTMLURL          string   `json:"html_url"`
	URL              string   `json:"url"`
	CloneURL         string   `json:"clone_url"`
	GitURL           string   `json:"git_url"`
	SSHURL           string   `json:"ssh_url"`
	Homepage         string   `json:"homepage"`
	Language         *string  `json:"language"`
	Topics           []string `json:"topics"`
	Visibility       string   `json:"visibility"`
	Fork             bool     `json:"fork"`
	Archived         bool     `json:"archived"`
	Disabled         bool     `json:"disabled"`
	HasIssues        bool     `json:"has_issues"`
	HasProjects      bool     `json:"has_projects"`
	HasWiki          bool     `json:"has_wiki"`
	Size             int      `json:"size"`
	StargazersCount  int      `json:"stargazers_count"`
	WatchersCount    int      `json:"watchers_count"`
	ForksCount       int      `json:"forks_count"`
	OpenIssuesCount  int      `json:"open_issues_count"`
	License          *string  `json:"license"`
	AllowSquashMerge bool     `json:"allow_squash_merge"`
	AllowMergeCommit bool     `json:"allow_merge_commit"`
	AllowRebaseMerge bool     `json:"allow_rebase_merge"`
}

func (s *store) repoToJSON(rp *repo) repoJSON {
	vis := "public"
	if rp.Private {
		vis = "private"
	}
	open := 0
	for _, is := range rp.Issues {
		if is.State == "open" {
			open++
		}
	}
	return repoJSON{
		ID: rp.ID, NodeID: globalID("Repository", rp.ID), Name: rp.Name,
		FullName: rp.Owner + "/" + rp.Name, Owner: viewerUserJSON(),
		Private: rp.Private, Description: rp.Description,
		DefaultBranch: rp.DefaultBranch,
		CreatedAt:     isoTime(rp.CreatedSeq), UpdatedAt: isoTime(rp.CreatedSeq),
		PushedAt: isoTime(rp.CreatedSeq),
		HTMLURL:  "https://example.invalid/" + rp.Owner + "/" + rp.Name,
		URL:      "/api/v3/repos/" + rp.Owner + "/" + rp.Name,
		CloneURL: "https://example.invalid/" + rp.Owner + "/" + rp.Name + ".git",
		GitURL:   "git://example.invalid/" + rp.Owner + "/" + rp.Name + ".git",
		SSHURL:   "git@example.invalid:" + rp.Owner + "/" + rp.Name + ".git",
		Topics:   []string{}, Visibility: vis,
		HasIssues: true, HasProjects: true, HasWiki: true,
		OpenIssuesCount:  open,
		AllowSquashMerge: true, AllowMergeCommit: true, AllowRebaseMerge: true,
	}
}

type labelJSON struct {
	Name        string `json:"name"`
	Color       string `json:"color"`
	Description string `json:"description"`
	Default     bool   `json:"default"`
	URL         string `json:"url"`
}

type issueJSON struct {
	ID                int         `json:"id"`
	NodeID            string      `json:"node_id"`
	Number            int         `json:"number"`
	Title             string      `json:"title"`
	Body              string      `json:"body"`
	State             string      `json:"state"`
	Labels            []labelJSON `json:"labels"`
	User              userJSON    `json:"user"`
	Assignee          *string     `json:"assignee"`
	Assignees         []string    `json:"assignees"`
	Milestone         *string     `json:"milestone"`
	Locked            bool        `json:"locked"`
	Comments          int         `json:"comments"`
	CreatedAt         string      `json:"created_at"`
	UpdatedAt         string      `json:"updated_at"`
	ClosedAt          *string     `json:"closed_at"`
	HTMLURL           string      `json:"html_url"`
	URL               string      `json:"url"`
	RepositoryURL     string      `json:"repository_url"`
	LabelsURL         string      `json:"labels_url"`
	CommentsURL       string      `json:"comments_url"`
	EventsURL         string      `json:"events_url"`
	AuthorAssociation string      `json:"author_association"`
}

func (s *store) issueToJSON(rp *repo, is *issue) issueJSON {
	labels := []labelJSON{}
	for _, name := range is.Labels {
		if lb, ok := rp.Labels[name]; ok {
			labels = append(labels, labelJSON{Name: lb.Name, Color: lb.Color, URL: "/api/v3/repos/" + rp.Owner + "/" + rp.Name + "/labels/" + lb.Name})
		} else {
			labels = append(labels, labelJSON{Name: name, Color: "ededed"})
		}
	}
	var closedAt *string
	if is.State == "closed" {
		t := is.UpdatedAt
		closedAt = &t
	}
	return issueJSON{
		ID: is.ID, NodeID: globalID("Issue", is.ID), Number: is.Number,
		Title: is.Title, Body: is.Body, State: is.State, Labels: labels,
		User: viewerUserJSON(), Assignees: []string{},
		Comments:  len(rp.Comments[is.Number]),
		CreatedAt: is.CreatedAt, UpdatedAt: is.UpdatedAt, ClosedAt: closedAt,
		HTMLURL:           "https://example.invalid/" + rp.Owner + "/" + rp.Name + "/issues/" + strconv.Itoa(is.Number),
		URL:               "/api/v3/repos/" + rp.Owner + "/" + rp.Name + "/issues/" + strconv.Itoa(is.Number),
		RepositoryURL:     "/api/v3/repos/" + rp.Owner + "/" + rp.Name,
		LabelsURL:         "/api/v3/repos/" + rp.Owner + "/" + rp.Name + "/issues/" + strconv.Itoa(is.Number) + "/labels{/name}",
		CommentsURL:       "/api/v3/repos/" + rp.Owner + "/" + rp.Name + "/issues/" + strconv.Itoa(is.Number) + "/comments",
		EventsURL:         "/api/v3/repos/" + rp.Owner + "/" + rp.Name + "/issues/" + strconv.Itoa(is.Number) + "/events",
		AuthorAssociation: "OWNER",
	}
}

// rebuildIssueCache membangun ulang JSON pre-serialized untuk satu issue.
func (s *store) rebuildIssueCache(rp *repo, is *issue) {
	is.cached = jsonBytes(s.issueToJSON(rp, is))
}

type pullJSON struct {
	ID             int         `json:"id"`
	NodeID         string      `json:"node_id"`
	Number         int         `json:"number"`
	Title          string      `json:"title"`
	Body           string      `json:"body"`
	State          string      `json:"state"`
	Locked         bool        `json:"locked"`
	Merged         bool        `json:"merged"`
	Mergeable      bool        `json:"mergeable"`
	MergedAt       *string     `json:"merged_at"`
	MergeCommitSHA string      `json:"merge_commit_sha"`
	Head           pullRefJSON `json:"head"`
	Base           pullRefJSON `json:"base"`
	User           userJSON    `json:"user"`
	CreatedAt      string      `json:"created_at"`
	UpdatedAt      string      `json:"updated_at"`
	ClosedAt       *string     `json:"closed_at"`
	HTMLURL        string      `json:"html_url"`
	URL            string      `json:"url"`
}

type pullRefJSON struct {
	Label string    `json:"label"`
	Ref   string    `json:"ref"`
	SHA   string    `json:"sha"`
	Repo  *repoJSON `json:"repo"`
	User  userJSON  `json:"user"`
}

func (s *store) pullToJSON(rp *repo, p *pull) pullJSON {
	mergeable, _ := s.pullConflict(rp, p)
	var mergedAt, closedAt *string
	if p.Merged {
		mergedAt = &p.MergedAt
		closedAt = &p.MergedAt
	} else if p.State == "closed" {
		closedAt = &p.UpdatedAt
	}
	rj := s.repoToJSON(rp)
	return pullJSON{
		ID: p.ID, NodeID: globalID("PullRequest", p.ID), Number: p.Number,
		Title: p.Title, Body: p.Body, State: p.State, Merged: p.Merged,
		Mergeable: !mergeable, MergedAt: mergedAt, MergeCommitSHA: p.MergeCommitSHA,
		Head: pullRefJSON{Label: rp.Owner + ":" + p.HeadRef, Ref: p.HeadRef, SHA: p.HeadSHA, Repo: &rj, User: viewerUserJSON()},
		Base: pullRefJSON{Label: rp.Owner + ":" + p.BaseRef, Ref: p.BaseRef, SHA: p.BaseSHA, Repo: &rj, User: viewerUserJSON()},
		User: viewerUserJSON(), CreatedAt: p.CreatedAt, UpdatedAt: p.UpdatedAt, ClosedAt: closedAt,
		HTMLURL: "https://example.invalid/" + rp.Owner + "/" + rp.Name + "/pull/" + strconv.Itoa(p.Number),
		URL:     "/api/v3/repos/" + rp.Owner + "/" + rp.Name + "/pulls/" + strconv.Itoa(p.Number),
	}
}

// ---------------------------------------------------------------------------
// Router REST
// ---------------------------------------------------------------------------

func handleREST(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/v3")
	path = strings.TrimSuffix(path, "/")
	if path == "" {
		if r.Method != "GET" {
			jsonErr(w, 404, "Not Found")
			return
		}
		w.Header().Set("X-OAuth-Scopes", "repo, read:org")
		jsonReply(w, 200, []byte(`{"current_user_url":"/api/v3/user","rate_limit_url":"/api/v3/rate_limit"}`))
		return
	}
	// Body JSON untuk metode tulis; malformed -> 400.
	var body map[string]any
	if r.Method == "POST" || r.Method == "PUT" || r.Method == "PATCH" {
		b, ok := readBody(r)
		if !ok {
			jsonErr(w, 400, "Bad Request")
			return
		}
		m, ok := decodeJSON(b)
		if !ok {
			jsonErr(w, 400, "Bad Request")
			return
		}
		body = m
	}
	seg := strings.Split(strings.TrimPrefix(path, "/"), "/")
	switch {
	case path == "/meta" && r.Method == "GET":
		jsonReply(w, 200, []byte(`{"installed_version":"1.0.0","version":"1.0.0"}`))
	case path == "/user" && r.Method == "GET":
		jsonReply(w, 200, jsonBytes(viewerUserJSON()))
	case path == "/user/repos":
		st.handleUserRepos(w, r, body)
	case len(seg) >= 2 && seg[0] == "repos":
		st.handleRepoRoutes(w, r, seg, body)
	default:
		jsonErr(w, 404, "Not Found")
	}
}

// handleUserRepos: GET list (pagination) dan POST create.
func (s *store) handleUserRepos(w http.ResponseWriter, r *http.Request, body map[string]any) {
	if r.Method == "GET" {
		q := r.URL.Query()
		perPage := intField(map[string]any{}, "per_page", 30)
		if v := q.Get("per_page"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				perPage = n
			}
		}
		page := 1
		if v := q.Get("page"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				page = n
			}
		}
		s.mu.RLock()
		total := len(s.order)
		start := (page - 1) * perPage
		if start > total {
			start = total
		}
		end := start + perPage
		if end > total {
			end = total
		}
		list := make([]repoJSON, 0, end-start)
		for _, rp := range s.order[start:end] {
			list = append(list, s.repoToJSON(rp))
		}
		s.mu.RUnlock()
		if end < total {
			nextURL := "http://" + r.Host + "/api/v3/user/repos?per_page=" + strconv.Itoa(perPage) + "&page=" + strconv.Itoa(page+1)
			w.Header().Set("Link", "<"+nextURL+">; rel=\"next\"")
		}
		jsonReply(w, 200, jsonBytes(list))
		return
	}
	if r.Method == "POST" {
		name := strField(body, "name")
		if name == "" {
			jsonErr(w, 422, "Validation Failed: name is required")
			return
		}
		s.mu.Lock()
		if _, exists := s.repos[viewerLogin+"/"+name]; exists {
			s.mu.Unlock()
			jsonErr(w, 422, "Validation Failed: repository already exists")
			return
		}
		seq := s.nextSeq()
		rp := &repo{
			ID: s.nextRepo, Owner: viewerLogin, Name: name,
			Description:   strField(body, "description"),
			Private:       boolField(body, "private", false),
			DefaultBranch: "main", CreatedSeq: seq,
			Refs: map[string]oid{}, Objects: map[oid]*gitObject{},
			IssuesByNum: map[int]*issue{}, PullsByNum: map[int]*pull{},
			NextIssue: 1, NextPull: 1,
			Labels: map[string]*label{}, Comments: map[int][]*comment{},
			packCache: map[string][]byte{},
		}
		s.nextRepo++
		s.repos[viewerLogin+"/"+name] = rp
		s.order = append(s.order, rp)
		out := s.repoToJSON(rp)
		s.mu.Unlock()
		jsonReply(w, 201, jsonBytes(out))
		return
	}
	jsonErr(w, 404, "Not Found")
}

// handleRepoRoutes: /repos/{owner}/{repo}[/...]
func (s *store) handleRepoRoutes(w http.ResponseWriter, r *http.Request, seg []string, body map[string]any) {
	owner, name := seg[1], ""
	if len(seg) >= 3 {
		name = seg[2]
	}
	rest := []string{}
	if len(seg) > 3 {
		rest = seg[3:]
	}
	// Repo harus milik viewer; nama sama di bawah owner lain -> 404.
	if owner != viewerLogin {
		jsonErr(w, 404, "Not Found")
		return
	}
	s.mu.RLock()
	rp := s.getRepo(owner, name)
	s.mu.RUnlock()
	if rp == nil {
		jsonErr(w, 404, "Not Found")
		return
	}
	if len(rest) == 0 {
		switch r.Method {
		case "GET":
			s.mu.RLock()
			out := s.repoToJSON(rp)
			s.mu.RUnlock()
			jsonReply(w, 200, jsonBytes(out))
		case "PATCH":
			s.mu.Lock()
			if d, ok := body["description"]; ok {
				if ds, ok := d.(string); ok {
					rp.Description = ds
				}
			}
			out := s.repoToJSON(rp)
			s.mu.Unlock()
			jsonReply(w, 200, jsonBytes(out))
		case "DELETE":
			s.mu.Lock()
			delete(s.repos, owner+"/"+name)
			for i, o := range s.order {
				if o == rp {
					s.order = append(s.order[:i], s.order[i+1:]...)
					break
				}
			}
			s.mu.Unlock()
			w.Header().Set("X-GitHub-Request-Id", reqID())
			w.WriteHeader(204)
		default:
			jsonErr(w, 404, "Not Found")
		}
		return
	}
	switch rest[0] {
	case "branches":
		if r.Method == "GET" && len(rest) == 2 {
			// GET /branches/{branch} — satu branch
			s.mu.RLock()
			tip, ok := rp.refTip("refs/heads/" + rest[1])
			s.mu.RUnlock()
			if !ok {
				jsonErr(w, 404, "Not Found")
				return
			}
			type branchJSON struct {
				Name   string `json:"name"`
				Commit struct {
					SHA string `json:"sha"`
					URL string `json:"url"`
				} `json:"commit"`
				Protected bool `json:"protected"`
			}
			var b branchJSON
			b.Name = rest[1]
			b.Commit.SHA = tip.hex()
			b.Commit.URL = "/api/v3/repos/" + owner + "/" + name + "/commits/" + tip.hex()
			jsonReply(w, 200, jsonBytes(b))
			return
		}
		if r.Method == "GET" {
			s.mu.RLock()
			type branchJSON struct {
				Name   string `json:"name"`
				Commit struct {
					SHA string `json:"sha"`
					URL string `json:"url"`
				} `json:"commit"`
				Protected bool `json:"protected"`
			}
			list := []branchJSON{}
			for _, n := range rp.sortedRefNames() {
				if !strings.HasPrefix(n, "refs/heads/") {
					continue
				}
				var b branchJSON
				b.Name = strings.TrimPrefix(n, "refs/heads/")
				b.Commit.SHA = rp.Refs[n].hex()
				b.Commit.URL = "/api/v3/repos/" + owner + "/" + name + "/commits/" + rp.Refs[n].hex()
				list = append(list, b)
			}
			s.mu.RUnlock()
			jsonReply(w, 200, jsonBytes(list))
			return
		}
	case "commits":
		if r.Method == "GET" {
			s.handleCommits(w, r, rp)
			return
		}
	case "contents":
		s.handleContents(w, r, rp, rest[1:], body)
		return
	case "labels":
		if r.Method == "POST" {
			lname := strField(body, "name")
			if lname == "" {
				jsonErr(w, 422, "Validation Failed: name is required")
				return
			}
			s.mu.Lock()
			if _, ok := rp.Labels[lname]; !ok {
				rp.Labels[lname] = &label{Name: lname, Color: strField(body, "color")}
				if rp.Labels[lname].Color == "" {
					rp.Labels[lname].Color = "ededed"
				}
				rp.LabelOrder = append(rp.LabelOrder, lname)
			}
			lb := rp.Labels[lname]
			s.mu.Unlock()
			jsonReply(w, 201, jsonBytes(labelJSON{Name: lb.Name, Color: lb.Color, URL: "/api/v3/repos/" + owner + "/" + name + "/labels/" + lb.Name}))
			return
		}
		if r.Method == "GET" {
			s.mu.RLock()
			list := []labelJSON{}
			for _, n := range rp.LabelOrder {
				lb := rp.Labels[n]
				list = append(list, labelJSON{Name: lb.Name, Color: lb.Color})
			}
			s.mu.RUnlock()
			jsonReply(w, 200, jsonBytes(list))
			return
		}
	case "issues":
		s.handleIssues(w, r, rp, rest[1:], body)
		return
	case "pulls":
		s.handlePulls(w, r, rp, rest[1:], body)
		return
	case "git":
		if len(rest) >= 2 && rest[1] == "refs" {
			s.handleGitRefs(w, r, rp, rest[2:], body)
			return
		}
	}
	jsonErr(w, 404, "Not Found")
}

// ---------------------------------------------------------------------------
// Commits
// ---------------------------------------------------------------------------

func (s *store) handleCommits(w http.ResponseWriter, r *http.Request, rp *repo) {
	shaQ := r.URL.Query().Get("sha")
	s.mu.RLock()
	defer s.mu.RUnlock()
	var tip oid
	var ok bool
	if shaQ == "" {
		tip, ok = rp.headOID()
	} else if strings.HasPrefix(shaQ, "refs/heads/") {
		tip, ok = rp.refTip(shaQ)
	} else if o, good := oidFromHex(shaQ); good {
		tip, ok = o, rp.Objects[o] != nil
	} else {
		tip, ok = rp.refTip("refs/heads/" + shaQ)
	}
	if !ok {
		jsonReply(w, 200, []byte("[]"))
		return
	}
	type commitJSON struct {
		SHA    string `json:"sha"`
		Commit struct {
			Message string `json:"message"`
			Author  struct {
				Name  string `json:"name"`
				Email string `json:"email"`
				Date  string `json:"date"`
			} `json:"author"`
			Committer struct {
				Name  string `json:"name"`
				Email string `json:"email"`
				Date  string `json:"date"`
			} `json:"committer"`
			Tree struct {
				SHA string `json:"sha"`
			} `json:"tree"`
		} `json:"commit"`
		Author    userJSON `json:"author"`
		Committer userJSON `json:"committer"`
		Parents   []struct {
			SHA string `json:"sha"`
		} `json:"parents"`
		URL     string `json:"url"`
		HTMLURL string `json:"html_url"`
	}
	list := []commitJSON{}
	seen := map[oid]bool{}
	queue := []oid{tip}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if seen[cur] {
			continue
		}
		seen[cur] = true
		o := rp.Objects[cur]
		if o == nil || o.typ != "commit" {
			continue
		}
		ci := parseCommit(o.data)
		var cj commitJSON
		cj.SHA = cur.hex()
		cj.Commit.Message = strings.TrimSuffix(ci.Message, "\n")
		cj.Commit.Author.Name = viewerLogin
		cj.Commit.Author.Email = viewerLogin + "@example.invalid"
		cj.Commit.Author.Date = isoTime(epochSec)
		cj.Commit.Committer = cj.Commit.Author
		cj.Commit.Tree.SHA = ci.Tree
		cj.Author = viewerUserJSON()
		cj.Committer = viewerUserJSON()
		for _, p := range ci.Parents {
			cj.Parents = append(cj.Parents, struct {
				SHA string `json:"sha"`
			}{SHA: p})
			if poid, ok := oidFromHex(p); ok {
				queue = append(queue, poid)
			}
		}
		cj.URL = "/api/v3/repos/" + rp.Owner + "/" + rp.Name + "/commits/" + cur.hex()
		cj.HTMLURL = "https://example.invalid/" + rp.Owner + "/" + rp.Name + "/commit/" + cur.hex()
		list = append(list, cj)
	}
	// tanggal commit: pakai timestamp dari objek commit bila bisa diparse
	for i := range list {
		if o := rp.Objects[mustOID(list[i].SHA)]; o != nil {
			ci := parseCommit(o.data)
			if d := commitDate(ci.Author); d != "" {
				list[i].Commit.Author.Date = d
				list[i].Commit.Committer.Date = d
			}
		}
	}
	jsonReply(w, 200, jsonBytes(list))
}

func mustOID(h string) oid {
	o, _ := oidFromHex(h)
	return o
}

// commitDate mengekstrak tanggal ISO dari "nama <email> <unix> +0000".
func commitDate(author string) string {
	parts := strings.Fields(author)
	if len(parts) < 3 {
		return ""
	}
	if n, err := strconv.ParseInt(parts[len(parts)-2], 10, 64); err == nil {
		// timestamp commit memakai epoch absolut dari skenario/bench
		return timeFormatUnix(n)
	}
	return ""
}

func timeFormatUnix(n int64) string {
	return time.Unix(n, 0).UTC().Format("2006-01-02T15:04:05Z")
}

// ---------------------------------------------------------------------------
// Contents
// ---------------------------------------------------------------------------

// subtreeOID menghitung oid tree untuk subdirektori dir di dalam flat map
// (path -> blob hex), tanpa menyimpan objek baru.
func subtreeOID(flat map[string]string, dir string) oid {
	prefix := dir + "/"
	sub := map[string]string{}
	for p, b := range flat {
		if strings.HasPrefix(p, prefix) {
			sub[p[len(prefix):]] = b
		}
	}
	var build func(paths map[string]string) oid
	build = func(paths map[string]string) oid {
		dirs := map[string]map[string]string{}
		var entries []treeEntry
		for p, blobOID := range paths {
			if idx := strings.Index(p, "/"); idx >= 0 {
				d := p[:idx]
				if dirs[d] == nil {
					dirs[d] = map[string]string{}
				}
				dirs[d][p[idx+1:]] = blobOID
			} else {
				entries = append(entries, treeEntry{Mode: "100644", Name: p, OID: blobOID})
			}
		}
		for d, subPaths := range dirs {
			subOID := build(subPaths)
			entries = append(entries, treeEntry{Mode: "40000", Name: d, OID: subOID.hex()})
		}
		id, _ := buildTree(entries)
		return id
	}
	return build(sub)
}

// resolveRefTip mengembalikan tip commit untuk ref (branch / sha / "").
func (s *store) resolveRefTip(rp *repo, ref string) (oid, bool) {
	if ref == "" {
		return rp.headOID()
	}
	if o, ok := oidFromHex(ref); ok {
		if rp.Objects[o] != nil {
			return o, true
		}
		return o, false
	}
	if strings.HasPrefix(ref, "refs/heads/") {
		return rp.refTip(ref)
	}
	return rp.refTip("refs/heads/" + ref)
}

// commitFiles membuat commit baru di branch dengan tree hasil perubahan.
func (s *store) commitFiles(rp *repo, branch, message string, files map[string]string, deletePaths []string) (oid, error) {
	seq := s.nextSeq()
	var parent oid
	var hasParent bool
	if branch != "" {
		if tip, ok := rp.refTip("refs/heads/" + branch); ok {
			parent, hasParent = tip, true
		}
	}
	if !hasParent {
		if tip, ok := rp.headOID(); ok {
			parent, hasParent = tip, true
		}
	}
	flat := map[string]string{}
	if hasParent {
		flat = rp.treeOfCommit(parent)
	}
	for p, blobOID := range files {
		flat[p] = blobOID
	}
	for _, p := range deletePaths {
		delete(flat, p)
	}
	treeOID := rp.buildTreeFromPaths(flat)
	ts := baseNow + seq
	ident := viewerLogin + " <" + viewerLogin + "@example.invalid> " + strconv.FormatInt(ts, 10) + " +0000"
	var b strings.Builder
	b.WriteString("tree " + treeOID.hex() + "\n")
	if hasParent {
		b.WriteString("parent " + parent.hex() + "\n")
	}
	b.WriteString("author " + ident + "\n")
	b.WriteString("committer " + ident + "\n")
	b.WriteString("\n" + message + "\n")
	data := []byte(b.String())
	cid := hashObject("commit", data)
	rp.Objects[cid] = &gitObject{typ: "commit", data: data}
	target := branch
	if target == "" {
		target = rp.DefaultBranch
	}
	rp.Refs["refs/heads/"+target] = cid
	s.clonePack = map[string][]byte{}
	return cid, nil
}

func (s *store) handleContents(w http.ResponseWriter, r *http.Request, rp *repo, rest []string, body map[string]any) {
	owner, name := rp.Owner, rp.Name
	filePath := strings.Join(rest, "/")
	switch r.Method {
	case "GET":
		ref := r.URL.Query().Get("ref")
		s.mu.RLock()
		tip, ok := s.resolveRefTip(rp, ref)
		if !ok {
			s.mu.RUnlock()
			jsonErr(w, 404, "Not Found")
			return
		}
		flat := rp.treeOfCommit(tip)
		// file?
		if blobHex, isFile := flat[filePath]; isFile {
			blobOID, _ := oidFromHex(blobHex)
			blob := rp.Objects[blobOID]
			s.mu.RUnlock()
			if blob == nil {
				jsonErr(w, 404, "Not Found")
				return
			}
			type contentJSON struct {
				Type     string `json:"type"`
				Encoding string `json:"encoding"`
				Content  string `json:"content"`
				SHA      string `json:"sha"`
				Size     int    `json:"size"`
				Name     string `json:"name"`
				Path     string `json:"path"`
				URL      string `json:"url"`
			}
			base := filePath
			if idx := strings.LastIndex(filePath, "/"); idx >= 0 {
				base = filePath[idx+1:]
			}
			jsonReply(w, 200, jsonBytes(contentJSON{
				Type: "file", Encoding: "base64",
				Content: base64.StdEncoding.EncodeToString(blob.data),
				SHA:     blobHex, Size: len(blob.data), Name: base, Path: filePath,
				URL: "/api/v3/repos/" + owner + "/" + name + "/contents/" + filePath,
			}))
			return
		}
		// direktori: entri langsung di bawah filePath
		type entryJSON struct {
			Type string `json:"type"`
			Name string `json:"name"`
			Path string `json:"path"`
			SHA  string `json:"sha"`
			Size int    `json:"size"`
			URL  string `json:"url"`
		}
		entries := []entryJSON{}
		prefix := filePath
		if prefix != "" {
			prefix += "/"
		}
		dirs := map[string]string{}
		for p, blobHex := range flat {
			if prefix != "" && !strings.HasPrefix(p, prefix) {
				continue
			}
			rel := p
			if prefix != "" {
				rel = p[len(prefix):]
			}
			if idx := strings.Index(rel, "/"); idx >= 0 {
				dir := rel[:idx]
				if _, ok := dirs[dir]; !ok {
					dirs[dir] = dir
				}
			} else {
				entries = append(entries, entryJSON{Type: "file", Name: rel, Path: p, SHA: blobHex, Size: len(rp.Objects[mustOID(blobHex)].data), URL: "/api/v3/repos/" + owner + "/" + name + "/contents/" + p})
			}
		}
		for dir := range dirs {
			// sha direktori = oid tree subdirektori yang sebenarnya
			entries = append(entries, entryJSON{Type: "dir", Name: dir, Path: prefix + dir, SHA: subtreeOID(flat, prefix+dir).hex(), URL: "/api/v3/repos/" + owner + "/" + name + "/contents/" + prefix + dir})
		}
		s.mu.RUnlock()
		if len(entries) == 0 {
			jsonErr(w, 404, "Not Found")
			return
		}
		// sha direktori yang benar: hitung dari tree di path itu
		sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
		jsonReply(w, 200, jsonBytes(entries))
	case "PUT":
		msg := strField(body, "message")
		if msg == "" {
			msg = "Update " + filePath
		}
		contentB64 := strField(body, "content")
		raw, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(contentB64, "\n", ""))
		if err != nil {
			raw, _ = base64.RawStdEncoding.DecodeString(contentB64)
		}
		branch := strField(body, "branch")
		shaArg := strField(body, "sha")
		s.mu.Lock()
		defer s.mu.Unlock()
		// cek file saat ini di branch target
		tip, hasTip := s.resolveRefTip(rp, branch)
		var curFlat map[string]string
		if hasTip {
			curFlat = rp.treeOfCommit(tip)
		} else {
			curFlat = map[string]string{}
		}
		curBlobHex, exists := curFlat[filePath]
		if exists && shaArg != "" && shaArg != curBlobHex {
			jsonErr(w, 409, "Conflict: sha does not match")
			return
		}
		if exists && shaArg == "" {
			// update tanpa sha pada file yang sudah ada: tolak seperti GitHub
			jsonErr(w, 409, "Conflict: sha is required to update a file")
			return
		}
		blobID := hashObject("blob", raw)
		rp.Objects[blobID] = &gitObject{typ: "blob", data: raw}
		commitID, _ := s.commitFiles(rp, branch, msg, map[string]string{filePath: blobID.hex()}, nil)
		status := 201
		if exists {
			status = 200
		}
		type putResp struct {
			Content struct {
				Name     string `json:"name"`
				Path     string `json:"path"`
				SHA      string `json:"sha"`
				Size     int    `json:"size"`
				Type     string `json:"type"`
				Encoding string `json:"encoding"`
				Content  string `json:"content"`
			} `json:"content"`
			Commit struct {
				SHA     string `json:"sha"`
				Message string `json:"message"`
			} `json:"commit"`
		}
		var resp putResp
		base := filePath
		if idx := strings.LastIndex(filePath, "/"); idx >= 0 {
			base = filePath[idx+1:]
		}
		resp.Content.Name = base
		resp.Content.Path = filePath
		resp.Content.SHA = blobID.hex()
		resp.Content.Size = len(raw)
		resp.Content.Type = "file"
		resp.Content.Encoding = "base64"
		resp.Content.Content = base64.StdEncoding.EncodeToString(raw)
		resp.Commit.SHA = commitID.hex()
		resp.Commit.Message = msg
		jsonReply(w, status, jsonBytes(resp))
	case "DELETE":
		// body DELETE dibaca longgar (boleh kosong)
		s.mu.Lock()
		defer s.mu.Unlock()
		branch := ""
		if body != nil {
			branch = strField(body, "branch")
		}
		tip, hasTip := s.resolveRefTip(rp, branch)
		if !hasTip {
			jsonErr(w, 404, "Not Found")
			return
		}
		flat := rp.treeOfCommit(tip)
		if _, exists := flat[filePath]; !exists {
			jsonErr(w, 404, "Not Found")
			return
		}
		msg := "Delete " + filePath
		if body != nil && strField(body, "message") != "" {
			msg = strField(body, "message")
		}
		commitID, _ := s.commitFiles(rp, branch, msg, nil, []string{filePath})
		type delResp struct {
			Commit struct {
				SHA string `json:"sha"`
			} `json:"commit"`
		}
		var resp delResp
		resp.Commit.SHA = commitID.hex()
		jsonReply(w, 200, jsonBytes(resp))
	default:
		jsonErr(w, 404, "Not Found")
	}
}

// ---------------------------------------------------------------------------
// Issues
// ---------------------------------------------------------------------------

func (s *store) handleIssues(w http.ResponseWriter, r *http.Request, rp *repo, rest []string, body map[string]any) {
	owner, name := rp.Owner, rp.Name
	if len(rest) == 0 {
		switch r.Method {
		case "GET":
			q := r.URL.Query()
			state := q.Get("state")
			if state == "" {
				state = "open"
			}
			sortDir := q.Get("direction")
			if sortDir == "" {
				sortDir = "desc"
			}
			s.mu.RLock()
			list := []*issue{}
			for _, is := range rp.Issues {
				if state != "all" && is.State != state {
					continue
				}
				list = append(list, is)
			}
			s.mu.RUnlock()
			if sortDir == "asc" {
				// sudah urutan naik (creation)
			} else {
				for i, j := 0, len(list)-1; i < j; i, j = i+1, j-1 {
					list[i], list[j] = list[j], list[i]
				}
			}
			// per_page/page sederhana
			out := make([]json.RawMessage, 0, len(list))
			for _, is := range list {
				out = append(out, is.cached)
			}
			jsonReply(w, 200, jsonBytes(out))
		case "POST":
			title := strField(body, "title")
			ib := strField(body, "body")
			var labelNames []string
			if v, ok := body["labels"]; ok {
				if arr, ok := v.([]any); ok {
					for _, e := range arr {
						switch t := e.(type) {
						case string:
							labelNames = append(labelNames, t)
						case map[string]any:
							if n := strField(t, "name"); n != "" {
								labelNames = append(labelNames, n)
							}
						}
					}
				}
			}
			s.mu.Lock()
			for _, ln := range labelNames {
				if _, ok := rp.Labels[ln]; !ok {
					s.mu.Unlock()
					jsonErr(w, 422, "Validation Failed: label \""+ln+"\" does not exist")
					return
				}
			}
			seq := s.nextSeq()
			num := rp.NextIssue
			rp.NextIssue++
			s.nextID++
			is := &issue{
				ID: s.nextID, Number: num, Title: title, Body: ib,
				State: "open", Labels: labelNames,
				CreatedAt: isoTime(seq), UpdatedAt: isoTime(seq), Seq: seq,
			}
			rp.Issues = append(rp.Issues, is)
			rp.IssuesByNum[num] = is
			s.rebuildIssueCache(rp, is)
			cached := is.cached
			s.mu.Unlock()
			_ = owner
			_ = name
			jsonReply(w, 201, cached)
		default:
			jsonErr(w, 404, "Not Found")
		}
		return
	}
	// /issues/{number}[/comments]
	num, err := strconv.Atoi(rest[0])
	if err != nil {
		jsonErr(w, 404, "Not Found")
		return
	}
	if len(rest) == 2 && rest[1] == "comments" {
		switch r.Method {
		case "GET":
			s.mu.RLock()
			cs := rp.Comments[num]
			type commentJSON struct {
				ID        int      `json:"id"`
				Body      string   `json:"body"`
				User      userJSON `json:"user"`
				CreatedAt string   `json:"created_at"`
				UpdatedAt string   `json:"updated_at"`
				HTMLURL   string   `json:"html_url"`
				URL       string   `json:"url"`
			}
			list := []commentJSON{}
			for _, c := range cs {
				list = append(list, commentJSON{ID: c.ID, Body: c.Body, User: viewerUserJSON(), CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt})
			}
			s.mu.RUnlock()
			jsonReply(w, 200, jsonBytes(list))
		case "POST":
			cbody := strField(body, "body")
			s.mu.Lock()
			is, ok := rp.IssuesByNum[num]
			if !ok {
				s.mu.Unlock()
				jsonErr(w, 404, "Not Found")
				return
			}
			_ = is
			rp.NextCommentID++
			seq := s.nextSeq()
			c := &comment{ID: rp.NextCommentID, Body: cbody, CreatedAt: isoTime(seq), UpdatedAt: isoTime(seq)}
			rp.Comments[num] = append(rp.Comments[num], c)
			s.mu.Unlock()
			type commentJSON struct {
				ID        int      `json:"id"`
				Body      string   `json:"body"`
				User      userJSON `json:"user"`
				CreatedAt string   `json:"created_at"`
				UpdatedAt string   `json:"updated_at"`
			}
			jsonReply(w, 201, jsonBytes(commentJSON{ID: c.ID, Body: c.Body, User: viewerUserJSON(), CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt}))
		default:
			jsonErr(w, 404, "Not Found")
		}
		return
	}
	if len(rest) == 1 {
		switch r.Method {
		case "GET":
			s.mu.RLock()
			is, ok := rp.IssuesByNum[num]
			if !ok {
				s.mu.RUnlock()
				jsonErr(w, 404, "Not Found")
				return
			}
			cached := is.cached
			s.mu.RUnlock()
			jsonReply(w, 200, cached)
		case "PATCH":
			s.mu.Lock()
			is, ok := rp.IssuesByNum[num]
			if !ok {
				s.mu.Unlock()
				jsonErr(w, 404, "Not Found")
				return
			}
			if stt := strField(body, "state"); stt == "open" || stt == "closed" {
				is.State = stt
			}
			if t := strField(body, "title"); t != "" {
				is.Title = t
			}
			if b, ok := body["body"]; ok {
				if bs, ok := b.(string); ok {
					is.Body = bs
				}
			}
			seq := s.nextSeq()
			is.UpdatedAt = isoTime(seq)
			s.rebuildIssueCache(rp, is)
			cached := is.cached
			s.mu.Unlock()
			jsonReply(w, 200, cached)
		default:
			jsonErr(w, 404, "Not Found")
		}
		return
	}
	jsonErr(w, 404, "Not Found")
}

// ---------------------------------------------------------------------------
// Pull requests
// ---------------------------------------------------------------------------

// pullConflict: apakah merge PR ini akan konflik (file yang diubah head juga
// diubah di base tip sejak merge-base, dengan isi berbeda).
func (s *store) pullConflict(rp *repo, p *pull) (bool, map[string]string) {
	baseTipOID, ok := rp.refTip("refs/heads/" + p.BaseRef)
	if !ok {
		return false, nil
	}
	baseTree := rp.treeOfCommit(mustOID(p.BaseSHA))
	headTree := rp.treeOfCommit(mustOID(p.HeadSHA))
	tipTree := rp.treeOfCommit(baseTipOID)
	conflict := false
	for path, headBlob := range headTree {
		baseBlob, inBase := baseTree[path]
		if !inBase {
			continue // file baru di head: tidak konflik
		}
		if headBlob == baseBlob {
			continue
		}
		tipBlob, inTip := tipTree[path]
		if !inTip {
			continue
		}
		if tipBlob != baseBlob && tipBlob != headBlob {
			conflict = true
		}
	}
	return conflict, nil
}

// computePullFiles menghitung diff merge-base -> head.
func (s *store) computePullFiles(rp *repo, p *pull) []pullFile {
	baseTree := rp.treeOfCommit(mustOID(p.BaseSHA))
	headTree := rp.treeOfCommit(mustOID(p.HeadSHA))
	paths := map[string]bool{}
	for k := range baseTree {
		paths[k] = true
	}
	for k := range headTree {
		paths[k] = true
	}
	names := make([]string, 0, len(paths))
	for k := range paths {
		names = append(names, k)
	}
	sort.Strings(names)
	files := []pullFile{}
	for _, path := range names {
		b, inBase := baseTree[path]
		h, inHead := headTree[path]
		if inBase && inHead && b == h {
			continue
		}
		status := "modified"
		add, del := 0, 0
		if !inBase {
			status = "added"
			if bo := rp.Objects[mustOID(h)]; bo != nil {
				add = countLines(bo.data)
			}
		} else if !inHead {
			status = "changed"
			if bo := rp.Objects[mustOID(b)]; bo != nil {
				del = countLines(bo.data)
			}
		} else {
			if bo := rp.Objects[mustOID(h)]; bo != nil {
				add = countLines(bo.data)
			}
			if bo := rp.Objects[mustOID(b)]; bo != nil {
				del = countLines(bo.data)
			}
			if add == 0 {
				add = 1
			}
		}
		files = append(files, pullFile{Filename: path, Status: status, Additions: add, Deletions: del, Changes: add + del})
	}
	return files
}

func countLines(b []byte) int {
	if len(b) == 0 {
		return 0
	}
	n := 0
	for _, c := range b {
		if c == '\n' {
			n++
		}
	}
	if b[len(b)-1] != '\n' {
		n++
	}
	return n
}

func (s *store) handlePulls(w http.ResponseWriter, r *http.Request, rp *repo, rest []string, body map[string]any) {
	if len(rest) == 0 {
		switch r.Method {
		case "GET":
			q := r.URL.Query()
			state := q.Get("state")
			if state == "" {
				state = "open"
			}
			s.mu.RLock()
			list := []pullJSON{}
			for _, p := range rp.Pulls {
				if state != "all" && p.State != state {
					continue
				}
				list = append(list, s.pullToJSON(rp, p))
			}
			s.mu.RUnlock()
			jsonReply(w, 200, jsonBytes(list))
		case "POST":
			head := normalizeRef(strField(body, "head"))
			base := normalizeRef(strField(body, "base"))
			if base == "" {
				base = rp.DefaultBranch
			}
			title := strField(body, "title")
			pbody := strField(body, "body")
			s.mu.Lock()
			headOID, headOK := rp.refTip("refs/heads/" + head)
			baseOID, baseOK := rp.refTip("refs/heads/" + base)
			if !headOK || !baseOK {
				s.mu.Unlock()
				jsonErr(w, 422, "Validation Failed: head or base branch does not exist")
				return
			}
			for _, p := range rp.Pulls {
				if p.State == "open" && p.HeadRef == head && p.BaseRef == base {
					s.mu.Unlock()
					jsonErr(w, 422, "Validation Failed: a pull request already exists for this branch")
					return
				}
			}
			seq := s.nextSeq()
			num := rp.NextPull
			rp.NextPull++
			s.nextID++
			p := &pull{
				ID: s.nextID, Number: num, Title: title, Body: pbody,
				State: "open", HeadRef: head, BaseRef: base,
				HeadSHA: headOID.hex(), BaseSHA: baseOID.hex(),
				CreatedAt: isoTime(seq), UpdatedAt: isoTime(seq), Seq: seq,
			}
			p.Files = s.computePullFiles(rp, p)
			rp.Pulls = append(rp.Pulls, p)
			rp.PullsByNum[num] = p
			out := s.pullToJSON(rp, p)
			s.mu.Unlock()
			jsonReply(w, 201, jsonBytes(out))
		default:
			jsonErr(w, 404, "Not Found")
		}
		return
	}
	num, err := strconv.Atoi(rest[0])
	if err != nil {
		jsonErr(w, 404, "Not Found")
		return
	}
	if len(rest) == 2 && rest[1] == "files" && r.Method == "GET" {
		s.mu.RLock()
		p, ok := rp.PullsByNum[num]
		if !ok {
			s.mu.RUnlock()
			jsonErr(w, 404, "Not Found")
			return
		}
		type fileJSON struct {
			Filename  string `json:"filename"`
			Status    string `json:"status"`
			Additions int    `json:"additions"`
			Deletions int    `json:"deletions"`
			Changes   int    `json:"changes"`
		}
		list := []fileJSON{}
		for _, f := range p.Files {
			list = append(list, fileJSON{Filename: f.Filename, Status: f.Status, Additions: f.Additions, Deletions: f.Deletions, Changes: f.Changes})
		}
		s.mu.RUnlock()
		jsonReply(w, 200, jsonBytes(list))
		return
	}
	if len(rest) == 2 && rest[1] == "merge" && r.Method == "PUT" {
		s.mu.Lock()
		p, ok := rp.PullsByNum[num]
		if !ok {
			s.mu.Unlock()
			jsonErr(w, 404, "Not Found")
			return
		}
		mergeID, status, msg := s.doMergePull(rp, p)
		s.mu.Unlock()
		if status == 405 {
			jsonErr(w, 405, msg)
			return
		}
		jsonReply(w, 200, jsonBytes(map[string]any{"sha": mergeID.hex(), "merged": true, "message": msg}))
		return
	}
	if len(rest) == 1 {
		switch r.Method {
		case "GET":
			s.mu.RLock()
			p, ok := rp.PullsByNum[num]
			if !ok {
				s.mu.RUnlock()
				jsonErr(w, 404, "Not Found")
				return
			}
			out := s.pullToJSON(rp, p)
			s.mu.RUnlock()
			jsonReply(w, 200, jsonBytes(out))
		default:
			jsonErr(w, 404, "Not Found")
		}
		return
	}
	jsonErr(w, 404, "Not Found")
}

func normalizeRef(ref string) string {
	ref = strings.TrimPrefix(ref, "refs/heads/")
	return ref
}

// ---------------------------------------------------------------------------
// git refs (REST)
// ---------------------------------------------------------------------------

func (s *store) handleGitRefs(w http.ResponseWriter, r *http.Request, rp *repo, rest []string, body map[string]any) {
	if r.Method == "POST" && len(rest) == 0 {
		ref := strField(body, "ref")
		sha := strField(body, "sha")
		if ref == "" || sha == "" {
			jsonErr(w, 422, "Validation Failed: ref and sha are required")
			return
		}
		oidVal, ok := oidFromHex(sha)
		if !ok {
			jsonErr(w, 422, "Validation Failed: sha is not a valid object id")
			return
		}
		s.mu.Lock()
		if rp.Objects[oidVal] == nil {
			s.mu.Unlock()
			jsonErr(w, 422, "Validation Failed: object does not exist")
			return
		}
		if _, exists := rp.Refs[ref]; exists {
			s.mu.Unlock()
			jsonErr(w, 422, "Validation Failed: reference already exists")
			return
		}
		rp.Refs[ref] = oidVal
		s.clonePack = map[string][]byte{}
		s.mu.Unlock()
		type refJSON struct {
			Ref    string `json:"ref"`
			URL    string `json:"url"`
			Object struct {
				SHA  string `json:"sha"`
				Type string `json:"type"`
				URL  string `json:"url"`
			} `json:"object"`
		}
		var out refJSON
		out.Ref = ref
		out.URL = "/api/v3/repos/" + rp.Owner + "/" + rp.Name + "/git/refs/" + strings.TrimPrefix(ref, "refs/")
		out.Object.SHA = sha
		out.Object.Type = "commit"
		jsonReply(w, 201, jsonBytes(out))
		return
	}
	if r.Method == "GET" {
		s.mu.RLock()
		defer s.mu.RUnlock()
		prefix := "refs/" + strings.Join(rest, "/")
		type refJSON struct {
			Ref    string `json:"ref"`
			Object struct {
				SHA  string `json:"sha"`
				Type string `json:"type"`
			} `json:"object"`
		}
		if oidVal, ok := rp.Refs[prefix]; ok {
			var out refJSON
			out.Ref = prefix
			out.Object.SHA = oidVal.hex()
			out.Object.Type = "commit"
			jsonReply(w, 200, jsonBytes(out))
			return
		}
		list := []refJSON{}
		for _, n := range rp.sortedRefNames() {
			if strings.HasPrefix(n, prefix) {
				var e refJSON
				e.Ref = n
				e.Object.SHA = rp.Refs[n].hex()
				e.Object.Type = "commit"
				list = append(list, e)
			}
		}
		if len(list) == 0 {
			jsonErr(w, 404, "Not Found")
			return
		}
		jsonReply(w, 200, jsonBytes(list))
		return
	}
	jsonErr(w, 404, "Not Found")
}
