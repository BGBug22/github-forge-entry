// Forge store: satu in-memory store untuk keempat permukaan (git, REST v3,
// GraphQL, MCP). Semua timestamp deterministik: dari MAKEFUN_TRIAL_NOW +
// counter monotonik, tidak pernah dari jam dinding. Viewer login direproduksi
// dari seed skenario publik kit (wire.ts SCENARIO_SEED) dengan RNG yang sama
// (xorshift64* + FNV-1a + splitmix64 finalizer), sehingga login token owner
// selalu "adaiversen" di setiap trial.
package main

import (
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// Deterministic clock
// ---------------------------------------------------------------------------

// epochSec adalah SCENARIO_EPOCH_SEC kit: 2025-01-01T00:00:00Z.
const epochSec int64 = 1735689600

// baseNow dibaca sekali dari MAKEFUN_TRIAL_NOW (RFC3339 atau detik unix);
// jatuh ke epochSec bila tidak ada. Semua created_at/updated_at/commit time
// adalah baseNow + seq detik, dengan seq monotonik naik per mutasi.
var baseNow int64 = epochSec

func initBaseNow() {
	v := os.Getenv("MAKEFUN_TRIAL_NOW")
	if v == "" {
		return
	}
	if n, err := strconv.ParseInt(v, 10, 64); err == nil {
		baseNow = n
		return
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		baseNow = t.Unix()
	}
}

func isoTime(seq int64) string {
	return time.Unix(baseNow+seq, 0).UTC().Format("2006-01-02T15:04:05Z")
}

// ---------------------------------------------------------------------------
// Scenario RNG (reproduksi wire.ts): viewer login token owner.
// ---------------------------------------------------------------------------

const scenarioSeed uint64 = 0x6a09e667f3bcc909

func fnv1a64(text string) uint64 {
	h := uint64(0xcbf29ce484222325)
	for i := 0; i < len(text); i++ {
		h ^= uint64(text[i])
		h *= 0x100000001b3
	}
	return h
}

func mix64(v uint64) uint64 {
	z := v + 0x9e3779b97f4a7c15
	z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
	z = (z ^ (z >> 27)) * 0x94d049bb133111eb
	return z ^ (z >> 31)
}

type rng struct{ state uint64 }

func newRng(label string) *rng {
	return &rng{state: mix64(scenarioSeed ^ fnv1a64(label))}
}

func (r *rng) nextU64() uint64 {
	x := r.state
	x ^= x >> 12
	x ^= x << 25
	x ^= x >> 27
	r.state = x
	return x * 0x2545f4914f6cdd1d
}

func (r *rng) nextU32() uint32 { return uint32(r.nextU64() >> 32) }

func (r *rng) nextInt(max int) int {
	if max == 1 {
		return 0
	}
	const range32 = 4294967296
	limit := range32 - (range32 % uint64(max))
	for {
		v := uint64(r.nextU32())
		if v < limit {
			return int(v % uint64(max))
		}
	}
}

func (r *rng) pick(items []string) string { return items[r.nextInt(len(items))] }

var firstNames = []string{"ada", "bram", "cora", "dmitri", "elena", "felix", "greta", "hugo", "iris", "jonas", "kira", "lars", "mira", "nico", "olga", "pavel", "quinn", "rosa", "sven", "tess", "ulla", "vito", "wren", "yara", "zane"}
var lastNames = []string{"anders", "bauer", "costa", "dahl", "eriksen", "fischer", "garcia", "holm", "iversen", "jansen", "keller", "lind", "moreau", "novak", "olsen", "peters", "quist", "rossi", "sato", "turi", "ulrich", "vega", "weber", "xu", "yilmaz", "zimmer"}

// scenarioUsers mengembalikan 12 login skenario dalam urutan wire.ts.
func scenarioUsers() []string {
	r := newRng("users")
	used := map[string]bool{}
	out := make([]string, 0, 12)
	for i := 0; i < 12; i++ {
		base := r.pick(firstNames) + r.pick(lastNames)
		login := base
		suffix := 2
		for used[login] {
			login = base + "-" + strconv.Itoa(suffix)
			suffix++
		}
		used[login] = true
		out = append(out, login)
	}
	return out
}

// viewerLogin adalah users[0] ("adaiversen"); strangerLogin users[2].
var viewerLogin = scenarioUsers()[0]
var strangerLogin = scenarioUsers()[2]

// ---------------------------------------------------------------------------
// Git objects
// ---------------------------------------------------------------------------

type oid [20]byte

func (o oid) hex() string { return hex.EncodeToString(o[:]) }

func oidFromHex(s string) (oid, bool) {
	var o oid
	if len(s) != 40 {
		return o, false
	}
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != 20 {
		return o, false
	}
	copy(o[:], b)
	return o, true
}

var zeroOID oid

func hashObject(typ string, data []byte) oid {
	h := sha1.New()
	h.Write([]byte(typ))
	h.Write([]byte(" "))
	h.Write([]byte(strconv.Itoa(len(data))))
	h.Write([]byte{0})
	h.Write(data)
	var o oid
	copy(o[:], h.Sum(nil))
	return o
}

type gitObject struct {
	typ  string // blob | tree | commit | tag
	data []byte
}

// ---------------------------------------------------------------------------
// Store types
// ---------------------------------------------------------------------------

type label struct {
	Name  string `json:"name"`
	Color string `json:"color"`
}

type issue struct {
	ID        int
	Number    int
	Title     string
	Body      string
	State     string // open | closed
	Labels    []string
	CreatedAt string
	UpdatedAt string
	Seq       int64
	cached    []byte // pre-serialized REST JSON (hot path read)
}

type pullFile struct {
	Filename  string
	Status    string
	Additions int
	Deletions int
	Changes   int
}

type pull struct {
	ID             int
	Number         int
	Title          string
	Body           string
	State          string // open | closed
	Merged         bool
	MergedAt       string
	MergeCommitSHA string
	HeadRef        string
	BaseRef        string
	HeadSHA        string
	BaseSHA        string // base tip saat PR dibuat (merge-base)
	Files          []pullFile
	CreatedAt      string
	UpdatedAt      string
	Seq            int64
}

type comment struct {
	ID        int
	Body      string
	CreatedAt string
	UpdatedAt string
}

type repo struct {
	ID            int
	Owner         string
	Name          string
	Description   string
	Private       bool
	DefaultBranch string
	CreatedSeq    int64
	Refs          map[string]oid // refs/heads/x, refs/tags/x
	Objects       map[oid]*gitObject
	Issues        []*issue
	IssuesByNum   map[int]*issue
	NextIssue     int
	Pulls         []*pull
	PullsByNum    map[int]*pull
	NextPull      int
	Labels        map[string]*label
	LabelOrder    []string
	Comments      map[int][]*comment
	NextCommentID int
	// fetchPackCache memoisasi packfile depth-1: key = head commit hex.
	packCache     map[string][]byte
	packCacheHead string
}

type store struct {
	mu        sync.RWMutex
	repos     map[string]*repo // key owner + "/" + name
	order     []*repo          // urutan pembuatan (untuk GET /user/repos)
	nextRepo  int
	nextID    int // id global untuk issue/PR id (stabil)
	seq       int64
	token     string
	reqSeq    uint64
	clonePack map[string][]byte // memo pack clone penuh per repo+tip
}

func newStore(token string) *store {
	return &store{
		repos:     map[string]*repo{},
		nextRepo:  5000,
		nextID:    100000,
		token:     token,
		clonePack: map[string][]byte{},
	}
}

// nextSeq mengambil nomor urut monotonik untuk timestamp deterministik.
func (s *store) nextSeq() int64 {
	s.seq++
	return s.seq
}

func (s *store) getRepo(owner, name string) *repo {
	r, ok := s.repos[owner+"/"+name]
	if !ok {
		return nil
	}
	return r
}

// globalID mengodekan id global GraphQL yang opaque (bukan databaseId polos).
func globalID(kind string, dbID int) string {
	return base64.StdEncoding.EncodeToString([]byte(kind + ":" + strconv.Itoa(dbID)))
}

// decodeGlobalID mengurai global id; mengembalikan kind dan databaseId.
func decodeGlobalID(id string) (string, int, bool) {
	b, err := base64.StdEncoding.DecodeString(id)
	if err != nil {
		return "", 0, false
	}
	parts := strings.SplitN(string(b), ":", 2)
	if len(parts) != 2 {
		return "", 0, false
	}
	n, err := strconv.Atoi(parts[1])
	if err != nil {
		return "", 0, false
	}
	return parts[0], n, true
}

// repoByDBID mencari repo dari databaseId numerik.
func (s *store) repoByDBID(dbID int) *repo {
	for _, r := range s.order {
		if r.ID == dbID {
			return r
		}
	}
	return nil
}

func (s *store) issueByGlobalID(id string) (*repo, *issue, bool) {
	kind, n, ok := decodeGlobalID(id)
	if !ok || kind != "Issue" {
		return nil, nil, false
	}
	for _, r := range s.order {
		for _, is := range r.Issues {
			if is.ID == n {
				return r, is, true
			}
		}
	}
	return nil, nil, false
}

func (s *store) pullByGlobalID(id string) (*repo, *pull, bool) {
	kind, n, ok := decodeGlobalID(id)
	if !ok || kind != "PullRequest" {
		return nil, nil, false
	}
	for _, r := range s.order {
		for _, p := range r.Pulls {
			if p.ID == n {
				return r, p, true
			}
		}
	}
	return nil, nil, false
}

// sortedRefNames mengembalikan nama ref terurut (deterministik).
func (r *repo) sortedRefNames() []string {
	names := make([]string, 0, len(r.Refs))
	for n := range r.Refs {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// refTip mengembalikan oid ref atau zero bila tidak ada.
func (r *repo) refTip(name string) (oid, bool) {
	o, ok := r.Refs[name]
	return o, ok
}

// headOID adalah tip default branch (atau zero bila repo kosong).
func (r *repo) headOID() (oid, bool) {
	return r.refTip("refs/heads/" + r.DefaultBranch)
}

// ---------------------------------------------------------------------------
// JSON helpers (cepat, tanpa alokasi berlebih di hot path)
// ---------------------------------------------------------------------------

func jsonBytes(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}
