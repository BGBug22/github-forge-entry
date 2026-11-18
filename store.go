package main

import (
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ---------- identifiers ----------

type OID [20]byte

func (o OID) Hex() string { return hex.EncodeToString(o[:]) }
func (o OID) IsZero() bool {
	for _, b := range o {
		if b != 0 {
			return false
		}
	}
	return true
}

func oidFromHex(s string) (OID, bool) {
	var o OID
	if len(s) != 40 {
		return o, false
	}
	b, err := hex.DecodeString(s)
	if err != nil {
		return o, false
	}
	copy(o[:], b)
	return o, true
}

// ---------- JSON shapes (shared by REST / GraphQL / MCP) ----------

type UserJSON struct {
	Login string `json:"login"`
	ID    int64  `json:"id"`
}

type LabelJSON struct {
	Name  string `json:"name"`
	Color string `json:"color"`
}

type RepoJSON struct {
	ID            int64    `json:"id"`
	Name          string   `json:"name"`
	FullName      string   `json:"full_name"`
	Private       bool     `json:"private"`
	Description   string   `json:"description"`
	DefaultBranch string   `json:"default_branch"`
	Owner         UserJSON `json:"owner"`
	HasIssues     bool     `json:"has_issues"`
	CreatedAt     string   `json:"created_at"`
	UpdatedAt     string   `json:"updated_at"`
}

type IssueJSON struct {
	ID        int64       `json:"id"`
	Number    int         `json:"number"`
	Title     string      `json:"title"`
	Body      string      `json:"body"`
	State     string      `json:"state"`
	Labels    []LabelJSON `json:"labels"`
	User      UserJSON    `json:"user"`
	CreatedAt string      `json:"created_at"`
	UpdatedAt string      `json:"updated_at"`
}

type PRJSON struct {
	ID             int64    `json:"id"`
	Number         int      `json:"number"`
	State          string   `json:"state"`
	Title          string   `json:"title"`
	Body           string   `json:"body"`
	Head           RefJSON  `json:"head"`
	Base           RefJSON  `json:"base"`
	Merged         bool     `json:"merged"`
	MergedAt       *string  `json:"merged_at"`
	MergeCommitSHA *string  `json:"merge_commit_sha"`
	User           UserJSON `json:"user"`
}

type RefJSON struct {
	Ref string `json:"ref"`
	SHA string `json:"sha"`
}

// ---------- domain objects ----------

type Label struct {
	Name  string
	Color string
}

type Comment struct {
	ID   int64
	Body string
}

type Issue struct {
	ID        int64
	Number    int
	Title     string
	Body      string
	State     string // open | closed
	Labels    []Label
	UserLogin string
	UserID    int64
	CreatedAt string
	UpdatedAt string
	Comments  []*Comment
	Cached    []byte // pre-serialized IssueJSON
	Repo      *Repo
}

type PR struct {
	ID                 int64
	Number             int
	Title              string
	Body               string
	State              string // open | closed
	HeadRef            string
	BaseRef            string
	HeadSHA            string // tip of head at creation
	BaseSHA            string // tip of base at creation
	BaseTipAtCreation  OID
	HeadTipAtCreation  OID
	Merged             bool
	MergedAt           string
	MergeCommitSHA     string
	UserLogin          string
	UserID             int64
	Repo               *Repo
}

type Repo struct {
	ID          int64
	Name        string
	OwnerLogin  string
	OwnerID     int64
	Private     bool
	Description string
	CreatedAt   string
	UpdatedAt   string
	Branches    map[string]OID
	BranchOrder []string
	Tags        map[string]OID // name -> object oid (commit or annotated tag)
	Issues      []*Issue
	IssueByNum  map[int]*Issue
	PRs         []*PR
	PRByNum     map[int]*PR
	Labels      []Label
	LabelSet    map[string]bool
	NextIssue   int
	NextPR      int
	// caches, invalidated on any ref/object change
	PackCache map[string][]byte
	AdvV0Up   []byte
	AdvV2     []byte
	AdvV0Recv []byte
}

type TreeEntry struct {
	Mode string
	Name string
	OID  OID
}

type CommitInfo struct {
	Tree      OID
	Parents   []OID
	Message   string
	Author    string
	AuthorEM  string
	AuthorTS  int64
	Committer string
	CommitEM  string
	CommitTS  int64
}

// ---------- store ----------

type Store struct {
	mu          sync.RWMutex
	Repos       map[string]*Repo
	RepoOrder   []*Repo
	Objects     map[OID][]byte // full raw object (header + content)
	CommitCache map[OID]*CommitInfo
	NextRepoID  int64
	NextIssueID int64
	NextPRID    int64
	NextComment int64
	Seq         int64
	Epoch       int64
	Token       string
	ReqSeq      int64
	packStarts  []packStart // transient, only during readPack
}

const (
	ViewerLogin = "adaiversen"
	ViewerID    = int64(1000)
	ViewerName  = "ada iversen"
	ViewerEmail = "adaiversen@example.invalid"
	TeammateID  = int64(1001)
	StrangerID  = int64(1002)
	DefaultEpoch = int64(1735689600) // 2025-01-01T00:00:00Z
)

func NewStore(epoch int64, token string) *Store {
	if epoch == 0 {
		epoch = DefaultEpoch
	}
	return &Store{
		Repos:       make(map[string]*Repo),
		Objects:     make(map[OID][]byte),
		CommitCache: make(map[OID]*CommitInfo),
		NextRepoID:  1,
		NextIssueID: 1,
		NextPRID:    1,
		NextComment: 1,
		Epoch:       epoch,
		Token:       token,
	}
}

func (s *Store) nextSeq() int64 { return atomic.AddInt64(&s.Seq, 1) }

func (s *Store) nextTS() int64 { return s.Epoch + s.nextSeq() }

func isoTime(ts int64) string {
	return time.Unix(ts, 0).UTC().Format("2006-01-02T15:04:05Z")
}

func (s *Store) nextReqID() int64 { return atomic.AddInt64(&s.ReqSeq, 1) }

// ---------- git objects ----------

func (s *Store) storeObject(typ string, content []byte) OID {
	hdr := typ + " " + strconv.Itoa(len(content)) + "\x00"
	raw := make([]byte, 0, len(hdr)+len(content))
	raw = append(raw, hdr...)
	raw = append(raw, content...)
	sum := sha1.Sum(raw)
	var o OID
	copy(o[:], sum[:])
	s.Objects[o] = raw
	return o
}

func (s *Store) objectContent(oid OID) ([]byte, bool) {
	raw, ok := s.Objects[oid]
	if !ok {
		return nil, false
	}
	i := bytes.IndexByte(raw, 0)
	if i < 0 {
		return nil, false
	}
	return raw[i+1:], true
}

func (s *Store) objectType(oid OID) string {
	raw, ok := s.Objects[oid]
	if !ok {
		return ""
	}
	i := bytes.IndexByte(raw, ' ')
	if i < 0 {
		return ""
	}
	return string(raw[:i])
}

func (s *Store) parseCommit(oid OID) *CommitInfo {
	// A tag (or any non-commit) is NOT a commit: parsing its bytes as a
	// commit yields a zero tree, which used to leak the zero OID into
	// fetch packs and corrupt their entry count.
	if s.objectType(oid) != "commit" {
		return nil
	}
	if ci, ok := s.CommitCache[oid]; ok {
		return ci
	}
	raw, ok := s.Objects[oid]
	if !ok {
		return nil
	}
	i := bytes.IndexByte(raw, 0)
	if i < 0 {
		return nil
	}
	body := string(raw[i+1:])
	ci := &CommitInfo{}
	head, msg, _ := strings.Cut(body, "\n\n")
	ci.Message = msg
	for _, line := range strings.Split(head, "\n") {
		switch {
		case strings.HasPrefix(line, "tree "):
			if o, ok := oidFromHex(strings.TrimSpace(line[5:])); ok {
				ci.Tree = o
			}
		case strings.HasPrefix(line, "parent "):
			if o, ok := oidFromHex(strings.TrimSpace(line[7:])); ok {
				ci.Parents = append(ci.Parents, o)
			}
		case strings.HasPrefix(line, "author "):
			ci.Author, ci.AuthorEM, ci.AuthorTS = parseIdent(line[7:])
		case strings.HasPrefix(line, "committer "):
			ci.Committer, ci.CommitEM, ci.CommitTS = parseIdent(line[10:])
		}
	}
	s.CommitCache[oid] = ci
	return ci
}

// "Name <email> 1234567890 +0000"
func parseIdent(v string) (string, string, int64) {
	lt := strings.Index(v, "<")
	gt := strings.Index(v, ">")
	if lt < 0 || gt < 0 {
		return strings.TrimSpace(v), "", 0
	}
	name := strings.TrimSpace(v[:lt])
	email := v[lt+1 : gt]
	rest := strings.Fields(strings.TrimSpace(v[gt+1:]))
	var ts int64
	if len(rest) > 0 {
		ts, _ = strconv.ParseInt(rest[0], 10, 64)
	}
	return name, email, ts
}

func (s *Store) parseTree(oid OID) []TreeEntry {
	content, ok := s.objectContent(oid)
	if !ok {
		return nil
	}
	var out []TreeEntry
	for len(content) > 0 {
		sp := bytes.IndexByte(content, ' ')
		if sp < 0 {
			break
		}
		mode := string(content[:sp])
		content = content[sp+1:]
		nul := bytes.IndexByte(content, 0)
		if nul < 0 || len(content) < nul+21 {
			break
		}
		var e TreeEntry
		e.Mode = mode
		e.Name = string(content[:nul])
		copy(e.OID[:], content[nul+1:nul+21])
		out = append(out, e)
		content = content[nul+21:]
	}
	return out
}

func (s *Store) flatFiles(treeOID OID) map[string]OID {
	out := make(map[string]OID)
	var walk func(oid OID, prefix string)
	walk = func(oid OID, prefix string) {
		for _, e := range s.parseTree(oid) {
			p := prefix + e.Name
			if e.Mode == "40000" {
				walk(e.OID, p+"/")
			} else {
				out[p] = e.OID
			}
		}
	}
	if !treeOID.IsZero() {
		walk(treeOID, "")
	}
	return out
}

func (s *Store) buildTreeRec(dir string, files map[string]OID) OID {
	// group immediate children of dir
	type child struct {
		name  string
		isDir bool
		oid   OID
	}
	seen := make(map[string]*child)
	for path, oid := range files {
		if !strings.HasPrefix(path, dir) {
			continue
		}
		rest := path[len(dir):]
		slash := strings.IndexByte(rest, '/')
		if slash < 0 {
			seen[rest] = &child{name: rest, oid: oid}
		} else {
			first := rest[:slash]
			if _, ok := seen[first]; !ok {
				seen[first] = &child{name: first, isDir: true}
			}
		}
	}
	var names []string
	for n := range seen {
		names = append(names, n)
	}
	// git tree order: names byte order, dirs compared as name+"/"
	sort.Slice(names, func(i, j int) bool {
		ki, kj := names[i], names[j]
		if seen[ki].isDir {
			ki += "/"
		}
		if seen[kj].isDir {
			kj += "/"
		}
		return ki < kj
	})
	var buf bytes.Buffer
	for _, n := range names {
		c := seen[n]
		if c.isDir {
			sub := s.buildTreeRec(dir+n+"/", files)
			c.oid = sub
			buf.WriteString("40000 " + n + "\x00")
		} else {
			buf.WriteString("100644 " + n + "\x00")
		}
		buf.Write(c.oid[:])
	}
	return s.storeObject("tree", buf.Bytes())
}

func (s *Store) buildTree(files map[string]OID) OID {
	return s.buildTreeRec("", files)
}

func (s *Store) makeCommit(tree OID, parents []OID, message, authorName, authorEmail string, ts int64) OID {
	if !strings.HasSuffix(message, "\n") {
		message += "\n"
	}
	var b strings.Builder
	b.WriteString("tree " + tree.Hex() + "\n")
	for _, p := range parents {
		b.WriteString("parent " + p.Hex() + "\n")
	}
	ident := authorName + " <" + authorEmail + "> " + strconv.FormatInt(ts, 10) + " +0000"
	b.WriteString("author " + ident + "\n")
	b.WriteString("committer " + ident + "\n\n")
	b.WriteString(message)
	return s.storeObject("commit", []byte(b.String()))
}

func (s *Store) isDescendant(newOID, oldOID OID) bool {
	if oldOID.IsZero() {
		return true
	}
	if newOID == oldOID {
		return true
	}
	seen := make(map[OID]bool)
	queue := []OID{newOID}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if seen[cur] {
			continue
		}
		seen[cur] = true
		if cur == oldOID {
			return true
		}
		if ci := s.parseCommit(cur); ci != nil {
			queue = append(queue, ci.Parents...)
		}
		if len(seen) > 20000 {
			break
		}
	}
	return false
}

func (s *Store) resolveTag(oid OID) OID {
	// annotated tag -> its commit; otherwise oid itself
	if s.objectType(oid) != "tag" {
		return oid
	}
	content, ok := s.objectContent(oid)
	if !ok {
		return oid
	}
	for _, line := range strings.Split(string(content), "\n") {
		if strings.HasPrefix(line, "object ") {
			if o, ok := oidFromHex(strings.TrimSpace(line[7:])); ok {
				return o
			}
		}
		if line == "" {
			break
		}
	}
	return oid
}

// ---------- repos / issues / PRs ----------

func repoKey(owner, name string) string { return owner + "/" + name }

func (s *Store) getRepo(owner, name string) *Repo {
	return s.Repos[repoKey(owner, name)]
}

func (s *Store) invalidate(r *Repo) {
	r.PackCache = make(map[string][]byte)
	r.AdvV0Up = nil
	r.AdvV2 = nil
	r.AdvV0Recv = nil
}

func (r *Repo) json() RepoJSON {
	return RepoJSON{
		ID: r.ID, Name: r.Name, FullName: r.OwnerLogin + "/" + r.Name,
		Private: r.Private, Description: r.Description, DefaultBranch: "main",
		Owner: UserJSON{Login: r.OwnerLogin, ID: r.OwnerID}, HasIssues: true,
		CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
	}
}

func (s *Store) createRepo(ownerLogin string, ownerID int64, name string, private bool, desc string) (*Repo, bool) {
	if s.getRepo(ownerLogin, name) != nil {
		return nil, false
	}
	ts := s.nextTS()
	r := &Repo{
		ID: s.NextRepoID, Name: name, OwnerLogin: ownerLogin, OwnerID: ownerID,
		Private: private, Description: desc,
		CreatedAt: isoTime(ts), UpdatedAt: isoTime(ts),
		Branches: make(map[string]OID), Tags: make(map[string]OID),
		IssueByNum: make(map[int]*Issue), PRByNum: make(map[int]*PR),
		LabelSet: make(map[string]bool), NextIssue: 1, NextPR: 1,
		PackCache: make(map[string][]byte),
	}
	s.NextRepoID++
	s.Repos[repoKey(ownerLogin, name)] = r
	s.RepoOrder = append(s.RepoOrder, r)
	return r, true
}

func (s *Store) rebuildIssueCache(iss *Issue) {
	labels := make([]LabelJSON, 0, len(iss.Labels))
	for _, l := range iss.Labels {
		labels = append(labels, LabelJSON{Name: l.Name, Color: l.Color})
	}
	j := IssueJSON{
		ID: iss.ID, Number: iss.Number, Title: iss.Title, Body: iss.Body,
		State: iss.State, Labels: labels,
		User: UserJSON{Login: iss.UserLogin, ID: iss.UserID},
		CreatedAt: iss.CreatedAt, UpdatedAt: iss.UpdatedAt,
	}
	b, _ := json.Marshal(j)
	iss.Cached = b
}

func (s *Store) createIssue(r *Repo, title, body string, labelNames []string) (*Issue, string) {
	var labels []Label
	for _, n := range labelNames {
		if !r.LabelSet[n] {
			return nil, "label does not exist: " + n
		}
		labels = append(labels, Label{Name: n, Color: labelColor(r, n)})
	}
	ts := s.nextTS()
	iss := &Issue{
		ID: s.NextIssueID, Number: r.NextIssue, Title: title, Body: body,
		State: "open", Labels: labels, UserLogin: ViewerLogin, UserID: ViewerID,
		CreatedAt: isoTime(ts), UpdatedAt: isoTime(ts), Repo: r,
	}
	s.NextIssueID++
	r.NextIssue++
	r.Issues = append(r.Issues, iss)
	r.IssueByNum[iss.Number] = iss
	s.rebuildIssueCache(iss)
	return iss, ""
}

func labelColor(r *Repo, name string) string {
	for _, l := range r.Labels {
		if l.Name == name {
			return l.Color
		}
	}
	return "000000"
}

func (s *Store) createPR(r *Repo, title, body, head, base string) (*PR, string) {
	headOID, ok := r.Branches[head]
	if !ok {
		return nil, "head branch does not exist"
	}
	baseOID, ok := r.Branches[base]
	if !ok {
		return nil, "base branch does not exist"
	}
	for _, p := range r.PRs {
		if p.State == "open" && p.HeadRef == head {
			return nil, "pull request already exists for head branch"
		}
	}
	pr := &PR{
		ID: s.NextPRID, Number: r.NextPR, Title: title, Body: body, State: "open",
		HeadRef: head, BaseRef: base, HeadSHA: headOID.Hex(), BaseSHA: baseOID.Hex(),
		BaseTipAtCreation: baseOID, HeadTipAtCreation: headOID,
		UserLogin: ViewerLogin, UserID: ViewerID, Repo: r,
	}
	s.NextPRID++
	r.NextPR++
	r.PRs = append(r.PRs, pr)
	r.PRByNum[pr.Number] = pr
	return pr, ""
}

func (s *Store) prConflicted(pr *PR) bool {
	r := pr.Repo
	baseTip, ok := r.Branches[pr.BaseRef]
	if !ok {
		return false
	}
	baseNow := s.flatFiles(s.parseCommit(baseTip).Tree)
	baseThen := s.flatFiles(s.parseCommit(pr.BaseTipAtCreation).Tree)
	headThen := s.flatFiles(s.parseCommit(pr.HeadTipAtCreation).Tree)
	changed := make(map[string]bool)
	for p, o := range headThen {
		if bo, ok := baseThen[p]; !ok || bo != o {
			changed[p] = true
		}
	}
	for p := range baseThen {
		if _, ok := headThen[p]; !ok {
			changed[p] = true
		}
	}
	for p := range changed {
		nowOID, nowOK := baseNow[p]
		thenOID, thenOK := baseThen[p]
		if nowOK != thenOK || nowOID != thenOID {
			return true
		}
	}
	return false
}

func (s *Store) prJSON(pr *PR) PRJSON {
	mergedAt := (*string)(nil)
	mergeSHA := (*string)(nil)
	if pr.Merged {
		mergedAt = &pr.MergedAt
		mergeSHA = &pr.MergeCommitSHA
	}
	return PRJSON{
		ID: pr.ID, Number: pr.Number, State: pr.State, Title: pr.Title, Body: pr.Body,
		Head: RefJSON{Ref: pr.HeadRef, SHA: pr.HeadSHA},
		Base: RefJSON{Ref: pr.BaseRef, SHA: pr.BaseSHA},
		Merged: pr.Merged, MergedAt: mergedAt, MergeCommitSHA: mergeSHA,
		User: UserJSON{Login: pr.UserLogin, ID: pr.UserID},
	}
}

func (s *Store) mergePR(pr *PR) (string, bool) {
	if pr.State != "open" {
		return "", false
	}
	if s.prConflicted(pr) {
		return "", false
	}
	r := pr.Repo
	baseTip := r.Branches[pr.BaseRef]
	headTip := r.Branches[pr.HeadRef]
	headCI := s.parseCommit(headTip)
	if headCI == nil {
		return "", false
	}
	ts := s.nextTS()
	msg := "Merge pull request #" + strconv.Itoa(pr.Number) + " from " + pr.HeadRef + "\n"
	sha := s.makeCommit(headCI.Tree, []OID{baseTip}, msg, ViewerName, ViewerEmail, ts)
	r.Branches[pr.BaseRef] = sha
	pr.State = "closed"
	pr.Merged = true
	pr.MergedAt = isoTime(ts)
	pr.MergeCommitSHA = sha.Hex()
	pr.HeadSHA = headTip.Hex()
	s.invalidate(r)
	return sha.Hex(), true
}

// ---------- contents (file = commit) ----------

// returns blobOID, commitOID, httpStatus, errMsg
func (s *Store) putContent(r *Repo, branch, path string, content []byte, message, sha string) (OID, OID, int, string) {
	if branch == "" {
		branch = "main"
	}
	tip, hasTip := r.Branches[branch]
	var files map[string]OID
	var parents []OID
	if hasTip {
		ci := s.parseCommit(tip)
		if ci == nil {
			return OID{}, OID{}, 500, "bad tip"
		}
		files = s.flatFiles(ci.Tree)
		parents = []OID{tip}
	} else {
		// unborn branch: start from default tip if it exists, else empty
		if dt, ok := r.Branches["main"]; ok && branch != "main" {
			ci := s.parseCommit(dt)
			files = s.flatFiles(ci.Tree)
			parents = []OID{dt}
		} else {
			files = make(map[string]OID)
		}
	}
	existed := false
	if cur, ok := files[path]; ok {
		existed = true
		if sha != "" && cur.Hex() != sha {
			return OID{}, OID{}, 409, "sha does not match current blob"
		}
	}
	blob := s.storeObject("blob", content)
	files[path] = blob
	tree := s.buildTree(files)
	ts := s.nextTS()
	if message == "" {
		message = "Update " + path
	}
	commit := s.makeCommit(tree, parents, message, ViewerName, ViewerEmail, ts)
	r.Branches[branch] = commit
	if !hasTip {
		// record branch order on first creation
		found := false
		for _, b := range r.BranchOrder {
			if b == branch {
				found = true
			}
		}
		if !found {
			r.BranchOrder = append(r.BranchOrder, branch)
		}
	}
	s.invalidate(r)
	if existed {
		return blob, commit, 200, ""
	}
	return blob, commit, 201, ""
}

func (s *Store) deleteContent(r *Repo, branch, path, message, sha string) (OID, int, string) {
	if branch == "" {
		branch = "main"
	}
	tip, ok := r.Branches[branch]
	if !ok {
		return OID{}, 404, "branch not found"
	}
	ci := s.parseCommit(tip)
	files := s.flatFiles(ci.Tree)
	cur, ok := files[path]
	if !ok {
		return OID{}, 404, "file not found"
	}
	if sha != "" && cur.Hex() != sha {
		return OID{}, 409, "sha does not match current blob"
	}
	delete(files, path)
	tree := s.buildTree(files)
	ts := s.nextTS()
	if message == "" {
		message = "Delete " + path
	}
	commit := s.makeCommit(tree, []OID{tip}, message, ViewerName, ViewerEmail, ts)
	r.Branches[branch] = commit
	s.invalidate(r)
	return commit, 200, ""
}

// tree oid of a directory path ("" = root) under a branch tip
func (s *Store) dirTreeOID(r *Repo, branch, dir string) (OID, bool) {
	tip, ok := r.Branches[branch]
	if !ok {
		return OID{}, false
	}
	ci := s.parseCommit(tip)
	if ci == nil {
		return OID{}, false
	}
	cur := ci.Tree
	if dir == "" {
		return cur, true
	}
	for _, part := range strings.Split(dir, "/") {
		found := false
		for _, e := range s.parseTree(cur) {
			if e.Name == part && e.Mode == "40000" {
				cur = e.OID
				found = true
				break
			}
		}
		if !found {
			return OID{}, false
		}
	}
	return cur, true
}

func countLines(b []byte) int {
	if len(b) == 0 {
		return 0
	}
	n := bytes.Count(b, []byte("\n"))
	if !bytes.HasSuffix(b, []byte("\n")) {
		n++
	}
	return n
}

type FileChange struct {
	Filename  string `json:"filename"`
	Status    string `json:"status"`
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
	Changes   int    `json:"changes"`
}

func (s *Store) prFiles(pr *PR) []FileChange {
	baseCI := s.parseCommit(pr.BaseTipAtCreation)
	headCI := s.parseCommit(pr.HeadTipAtCreation)
	if baseCI == nil || headCI == nil {
		return []FileChange{}
	}
	base := s.flatFiles(baseCI.Tree)
	head := s.flatFiles(headCI.Tree)
	paths := make(map[string]bool)
	for p := range base {
		paths[p] = true
	}
	for p := range head {
		paths[p] = true
	}
	var names []string
	for p := range paths {
		names = append(names, p)
	}
	sort.Strings(names)
	var out []FileChange
	for _, p := range names {
		bo, inBase := base[p]
		ho, inHead := head[p]
		if inBase && inHead && bo == ho {
			continue
		}
		fc := FileChange{Filename: p}
		if !inBase {
			fc.Status = "added"
			if c, ok := s.objectContent(ho); ok {
				fc.Additions = countLines(c)
			}
			if fc.Additions < 1 {
				fc.Additions = 1
			}
		} else if !inHead {
			fc.Status = "removed"
			if c, ok := s.objectContent(bo); ok {
				fc.Deletions = countLines(c)
			}
		} else {
			fc.Status = "modified"
			var oldN, newN int
			if c, ok := s.objectContent(bo); ok {
				oldN = countLines(c)
			}
			if c, ok := s.objectContent(ho); ok {
				newN = countLines(c)
			}
			if newN >= oldN {
				fc.Additions = newN - oldN
				if fc.Additions < 1 {
					fc.Additions = 1
				}
			} else {
				fc.Additions = 1
				fc.Deletions = oldN - newN
			}
		}
		fc.Changes = fc.Additions + fc.Deletions
		out = append(out, fc)
	}
	if out == nil {
		out = []FileChange{}
	}
	return out
}

// global id helpers (Relay, opaque)
func globalID(kind string, parts ...string) string {
	return encodeB64(kind + ":" + strings.Join(parts, ":"))
}

func encodeB64(s string) string {
	return b64Encode([]byte(s))
}

// decodeGlobalID returns kind and parts, or false.
func decodeGlobalID(id string) (string, []string, bool) {
	b, ok := b64Decode(id)
	if !ok {
		return "", nil, false
	}
	parts := strings.Split(string(b), ":")
	if len(parts) < 2 {
		return "", nil, false
	}
	return parts[0], parts[1:], true
}