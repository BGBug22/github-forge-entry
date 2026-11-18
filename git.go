package main

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/binary"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

// ---------- pkt-line ----------

func pktLine(s string) []byte {
	n := len(s) + 4
	return append([]byte(fmt.Sprintf("%04x", n)), s...)
}

var pktFlush = []byte("0000")
var pktDelim = []byte("0001")

func pktLines(data []byte) []string {
	var out []string
	for len(data) >= 4 {
		if string(data[:4]) == "0000" || string(data[:4]) == "0001" || string(data[:4]) == "0002" {
			data = data[4:]
			continue
		}
		n, err := strconv.ParseInt(string(data[:4]), 16, 32)
		if err != nil || n < 4 || int(n) > len(data) {
			break
		}
		out = append(out, string(data[4:n]))
		data = data[n:]
	}
	return out
}

// split body into pkt-line command section and the rest (pack bytes)
func splitPackBody(data []byte) ([]string, []byte) {
	for i := 0; i+4 <= len(data); {
		if string(data[i:i+4]) == "0000" {
			return pktLines(data[:i]), data[i+4:]
		}
		n, err := strconv.ParseInt(string(data[i:i+4]), 16, 32)
		if err != nil || n < 4 {
			break
		}
		i += int(n)
	}
	return pktLines(data), nil
}

func writeSideband(w io.Writer, band byte, data []byte) {
	// chunk data into sideband packets: 1 band byte + up to 65515 data bytes
	for len(data) > 0 {
		chunk := 65515
		if len(data) < chunk {
			chunk = len(data)
		}
		payload := make([]byte, 0, chunk+1)
		payload = append(payload, band)
		payload = append(payload, data[:chunk]...)
		w.Write([]byte(fmt.Sprintf("%04x", len(payload)+4)))
		w.Write(payload)
		data = data[chunk:]
	}
}

// ---------- pack reading ----------

type packObject struct {
	typ     int
	content []byte
	oid     OID
}

func readVarint(data []byte, pos *int) (uint64, bool) {
	var v uint64
	var shift uint
	for *pos < len(data) {
		b := data[*pos]
		*pos++
		v |= uint64(b&0x7f) << shift
		shift += 7
		if b&0x80 == 0 {
			return v, true
		}
		if shift > 63 {
			return 0, false
		}
	}
	return 0, false
}

func readPackHeader(data []byte, pos *int) (typ int, size uint64, ok bool) {
	if *pos >= len(data) {
		return 0, 0, false
	}
	b := data[*pos]
	*pos++
	typ = int((b >> 4) & 0x7)
	size = uint64(b & 0x0f)
	shift := uint(4)
	for b&0x80 != 0 {
		if *pos >= len(data) {
			return 0, 0, false
		}
		b = data[*pos]
		*pos++
		size |= uint64(b&0x7f) << shift
		shift += 7
	}
	return typ, size, true
}

func readOFSOffset(data []byte, pos *int) (int, bool) {
	if *pos >= len(data) {
		return 0, false
	}
	b := data[*pos]
	*pos++
	v := int(b & 0x7f)
	for b&0x80 != 0 {
		if *pos >= len(data) {
			return 0, false
		}
		b = data[*pos]
		*pos++
		v = ((v + 1) << 7) | int(b&0x7f)
	}
	return v, true
}

func zlibInflateOnce(data []byte, pos *int) ([]byte, int, bool) {
	// Inflate once, counting consumed bytes exactly. The counting reader
	// MUST implement io.ByteReader (ReadByte): without it, flate wraps
	// the reader in a bufio and over-reads past the end of the zlib
	// stream, corrupting every later object in the pack. With ReadByte,
	// flate pulls byte-by-byte and the count is exact.
	cr := &countingReader{r: bytes.NewReader(data[*pos:])}
	zr, err := zlib.NewReader(cr)
	if err != nil {
		return nil, 0, false
	}
	out, err := io.ReadAll(zr)
	zr.Close()
	if err != nil {
		return nil, 0, false
	}
	*pos += cr.n
	return out, cr.n, true
}

type countingReader struct {
	r *bytes.Reader
	n int
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += n
	return n, err
}

// ReadByte makes this an io.ByteReader, so flate/zlib never buffer ahead.
func (c *countingReader) ReadByte() (byte, error) {
	b, err := c.r.ReadByte()
	if err == nil {
		c.n++
	}
	return b, err
}

func applyDelta(base, delta []byte) ([]byte, bool) {
	pos := 0
	srcSize, ok := readVarint(delta, &pos)
	if !ok || uint64(len(base)) != srcSize {
		return nil, false
	}
	tgtSize, ok := readVarint(delta, &pos)
	if !ok {
		return nil, false
	}
	out := make([]byte, 0, tgtSize)
	for pos < len(delta) {
		op := delta[pos]
		pos++
		if op&0x80 != 0 {
			var off, size uint64
			for i := 0; i < 4; i++ {
				if op&(1<<uint(i)) != 0 {
					if pos >= len(delta) {
						return nil, false
					}
					off |= uint64(delta[pos]) << uint(8*i)
					pos++
				}
			}
			for i := 0; i < 3; i++ {
				if op&(0x10<<uint(i)) != 0 {
					if pos >= len(delta) {
						return nil, false
					}
					size |= uint64(delta[pos]) << uint(8*i)
					pos++
				}
			}
			if size == 0 {
				size = 0x10000
			}
			if off+size > uint64(len(base)) {
				return nil, false
			}
			out = append(out, base[off:off+size]...)
		} else if op != 0 {
			if pos+int(op) > len(delta) {
				return nil, false
			}
			out = append(out, delta[pos:pos+int(op)]...)
			pos += int(op)
		} else {
			return nil, false
		}
	}
	if uint64(len(out)) != tgtSize {
		return nil, false
	}
	return out, true
}

var packTypeName = map[int]string{1: "commit", 2: "tree", 3: "blob", 4: "tag"}
var packTypeNum = map[string]int{"commit": 1, "tree": 2, "blob": 3, "tag": 4}

// readPack stores every object into the store and returns the oids in pack order.
func (s *Store) readPack(data []byte) ([]OID, bool) {
	if len(data) < 12 || string(data[:4]) != "PACK" {
		return nil, false
	}
	ver := binary.BigEndian.Uint32(data[4:8])
	count := binary.BigEndian.Uint32(data[8:12])
	if ver != 2 && ver != 3 {
		return nil, false
	}
	pos := 12
	var oids []OID
	// oids by pack position (for OFS_DELTA)
	posOIDs := make([]OID, 0, count)
	for i := uint32(0); i < count; i++ {
		startPos := pos
		typ, _, ok := readPackHeader(data, &pos)
		if !ok {
			return nil, false
		}
		var objTyp string
		var content []byte
		switch typ {
		case 1, 2, 3, 4:
			objTyp = packTypeName[typ]
			c, _, ok := zlibInflateOnce(data, &pos)
			if !ok {
				return nil, false
			}
			content = c
		case 6: // OFS_DELTA
			off, ok := readOFSOffset(data, &pos)
			if !ok {
				return nil, false
			}
			basePos := startPos - off
			if basePos < 0 {
				return nil, false
			}
			// find base oid: the object whose pack entry started at basePos.
			// We track start positions in a side list via posOIDs index scan is
			// avoided by re-deriving: base must be an earlier object; find it by
			// matching start positions recorded below.
			baseOID, ok := s.packOIDByStart(basePos)
			if !ok {
				return nil, false
			}
			delta, _, ok := zlibInflateOnce(data, &pos)
			if !ok {
				return nil, false
			}
			baseContent, ok := s.objectContent(baseOID)
			if !ok {
				return nil, false
			}
			objTyp = s.objectType(baseOID)
			c, ok := applyDelta(baseContent, delta)
			if !ok {
				return nil, false
			}
			content = c
		case 7: // REF_DELTA
			if pos+20 > len(data) {
				return nil, false
			}
			var baseOID OID
			copy(baseOID[:], data[pos:pos+20])
			pos += 20
			delta, _, ok := zlibInflateOnce(data, &pos)
			if !ok {
				return nil, false
			}
			baseContent, ok := s.objectContent(baseOID)
			if !ok {
				return nil, false
			}
			objTyp = s.objectType(baseOID)
			c, ok := applyDelta(baseContent, delta)
			if !ok {
				return nil, false
			}
			content = c
		default:
			return nil, false
		}
		oid := s.storeObject(objTyp, content)
		s.packStarts = append(s.packStarts, packStart{pos: startPos, oid: oid})
		posOIDs = append(posOIDs, oid)
		oids = append(oids, oid)
		_ = posOIDs
	}
	// trailer: 20-byte SHA-1, not verified beyond length (objects verified by sha1 on store)
	return oids, true
}

type packStart struct {
	pos int
	oid OID
}

func (s *Store) packOIDByStart(pos int) (OID, bool) {
	for _, ps := range s.packStarts {
		if ps.pos == pos {
			return ps.oid, true
		}
	}
	return OID{}, false
}

// ---------- pack writing ----------

func packEntryHeader(typ int, size uint64) []byte {
	var out []byte
	b0 := byte((typ << 4) | int(size&0x0f))
	size >>= 4
	if size > 0 {
		b0 |= 0x80
	}
	out = append(out, b0)
	for size > 0 {
		b := byte(size & 0x7f)
		size >>= 7
		if size > 0 {
			b |= 0x80
		}
		out = append(out, b)
	}
	return out
}

func (s *Store) buildPack(oids []OID) []byte {
	// deterministic order: commits, trees, tags, blobs; then oid hex
	ord := func(o OID) int {
		switch s.objectType(o) {
		case "commit":
			return 0
		case "tree":
			return 1
		case "tag":
			return 2
		default:
			return 3
		}
	}
	sorted := append([]OID(nil), oids...)
	sort.Slice(sorted, func(i, j int) bool {
		if ord(sorted[i]) != ord(sorted[j]) {
			return ord(sorted[i]) < ord(sorted[j])
		}
		return sorted[i].Hex() < sorted[j].Hex()
	})
	var buf bytes.Buffer
	buf.WriteString("PACK")
	var hdr [8]byte
	binary.BigEndian.PutUint32(hdr[0:4], 2)
	// Entries are built first so the declared count is the count actually
	// written: a missing object must never leave the count short, or the
	// client's pack parser runs into the trailer and fails.
	var entries bytes.Buffer
	count := 0
	for _, o := range sorted {
		content, ok := s.objectContent(o)
		if !ok {
			continue
		}
		typ := packTypeNum[s.objectType(o)]
		entries.Write(packEntryHeader(typ, uint64(len(content))))
		var zb bytes.Buffer
		zw, _ := zlib.NewWriterLevel(&zb, zlib.BestSpeed)
		zw.Write(content)
		zw.Close()
		entries.Write(zb.Bytes())
		count++
	}
	binary.BigEndian.PutUint32(hdr[4:8], uint32(count))
	buf.Write(hdr[:])
	buf.Write(entries.Bytes())
	// trailer sha1 over everything so far
	sum := sha1Sum(buf.Bytes())
	buf.Write(sum[:])
	return buf.Bytes()
}

func sha1Sum(b []byte) [20]byte {
	return sha1.Sum(b)
}

// collect objects reachable from wants, minus haves, with optional depth limit.
// Returns object oids, shallow boundary commits, unshallow commits.
// deepen=true means the client asked to extend an existing shallow
// boundary: walk THROUGH the old boundary commits up to maxDepth from the
// wants (absolute depth), instead of stopping at them.
func (s *Store) collectFetch(wants []OID, haves []OID, shallowReq []OID, maxDepth int, deepen bool) ([]OID, []OID, []OID) {
	shallowSet := make(map[OID]bool)
	for _, o := range shallowReq {
		shallowSet[o] = true
	}
	haveReach := make(map[OID]bool)
	// haves reachable (unlimited), except: for a deepen fetch, do not
	// descend past the old shallow boundary — its ancestors are exactly
	// what the client is asking for.
	queue := append([]OID(nil), haves...)
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if haveReach[cur] {
			continue
		}
		haveReach[cur] = true
		if deepen && shallowSet[cur] {
			continue
		}
		if ci := s.parseCommit(cur); ci != nil {
			queue = append(queue, ci.Parents...)
		}
	}
	// Full object closure of the haves (commits, trees, blobs, tags):
	// an incremental fetch must not re-send objects the client already
	// holds, including blobs unchanged between the old and new tip.
	haveObjs := make(map[OID]bool)
	var expandHave func(oid OID)
	expandHave = func(oid OID) {
		if oid.IsZero() || haveObjs[oid] {
			return
		}
		haveObjs[oid] = true
		switch s.objectType(oid) {
		case "commit":
			if ci := s.parseCommit(oid); ci != nil {
				expandHave(ci.Tree)
			}
		case "tag":
			expandHave(s.resolveTag(oid))
		case "tree":
			for _, e := range s.parseTree(oid) {
				expandHave(e.OID)
			}
		}
	}
	for _, h := range haves {
		expandHave(h)
	}
	included := make(map[OID]bool) // commits included
	dist := make(map[OID]int)
	q := append([]OID(nil), wants...)
	for _, w := range wants {
		dist[w] = 0
	}
	for len(q) > 0 {
		cur := q[0]
		q = q[1:]
		d := dist[cur]
		if s.objectType(cur) == "tag" {
			// include tag, then traverse its target
			included[cur] = true
			t := s.resolveTag(cur)
			if t != cur && !haveReach[t] {
				if _, ok := dist[t]; !ok {
					dist[t] = d
					q = append(q, t)
				}
			}
			continue
		}
		ci := s.parseCommit(cur)
		if ci == nil {
			continue
		}
		if haveReach[cur] {
			// Already held — but for a deepen fetch, an old shallow
			// boundary is held WITHOUT its history: walk its parents
			// (without re-sending the boundary itself) so the new
			// generation is delivered.
			if deepen && shallowSet[cur] {
				if maxDepth > 0 && d+1 >= maxDepth {
					continue
				}
				for _, p := range ci.Parents {
					if _, ok := dist[p]; !ok {
						dist[p] = d + 1
						q = append(q, p)
					}
				}
			}
			continue
		}
		included[cur] = true
		if shallowSet[cur] && !deepen {
			continue // already shallow: do not walk parents
		}
		if maxDepth > 0 && d+1 >= maxDepth {
			continue
		}
		for _, p := range ci.Parents {
			if _, ok := dist[p]; !ok {
				dist[p] = d + 1
				q = append(q, p)
			}
		}
	}
	var shallow, unshallow []OID
	objSet := make(map[OID]bool)
	var addTree func(oid OID)
	addTree = func(oid OID) {
		if objSet[oid] || haveObjs[oid] {
			return
		}
		objSet[oid] = true
		for _, e := range s.parseTree(oid) {
			if haveObjs[e.OID] {
				continue
			}
			if e.Mode == "40000" {
				addTree(e.OID)
			} else {
				objSet[e.OID] = true
			}
		}
	}
	for c := range included {
		if haveObjs[c] {
			continue
		}
		objSet[c] = true
		ci := s.parseCommit(c)
		if ci != nil {
			addTree(ci.Tree)
		} else if s.objectType(c) == "tag" {
			// tag object itself
			objSet[c] = true
		}
		// boundary: has a parent not included, or is the root of a
		// depth-limited walk (a depth fetch that reaches the first
		// commit still reports it as the shallow boundary).
		if ci != nil {
			boundary := false
			for _, p := range ci.Parents {
				if !included[p] && !haveReach[p] {
					boundary = true
				}
			}
			if len(ci.Parents) == 0 && maxDepth > 0 {
				boundary = true
			}
			if boundary {
				shallow = append(shallow, c)
			}
			if shallowSet[c] {
				unshallow = append(unshallow, c)
			}
		}
	}
	// unshallow: previously shallow commits that are now included with parents
	// (client sent shallow <oid> and deepen asks beyond it): those in shallowReq
	// that are included and whose parents are now included.
	var unsh []OID
	for _, o := range shallowReq {
		if included[o] {
			if ci := s.parseCommit(o); ci != nil {
				for _, p := range ci.Parents {
					if included[p] {
						unsh = append(unsh, o)
						break
					}
				}
			}
		}
	}
	if len(unsh) > 0 {
		unshallow = unsh
	}
	var objs []OID
	for o := range objSet {
		objs = append(objs, o)
	}
	sort.Slice(shallow, func(i, j int) bool { return shallow[i].Hex() < shallow[j].Hex() })
	sort.Slice(unshallow, func(i, j int) bool { return unshallow[i].Hex() < unshallow[j].Hex() })
	return objs, shallow, unshallow
}

// ---------- git HTTP ----------

func (s *Store) gitAuth(r *http.Request) bool {
	if s.Token == "" {
		return true
	}
	return r.Header.Get("Authorization") == "Bearer "+s.Token
}

func gitNotFound(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(404)
	w.Write([]byte(`{"message":"Not Found"}`))
}

func (s *Store) parseGitPath(path string) (owner, repo string, rest string, ok bool) {
	// /{owner}/{repo}.git/...
	if !strings.HasPrefix(path, "/") {
		return "", "", "", false
	}
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	if len(parts) < 2 || !strings.HasSuffix(parts[1], ".git") {
		return "", "", "", false
	}
	owner = parts[0]
	repo = strings.TrimSuffix(parts[1], ".git")
	rest = strings.Join(parts[2:], "/")
	return owner, repo, rest, true
}

func (s *Store) allRefs(r *Repo) (branches []string, tags []string) {
	// Only branches that still exist in the map are advertised; a stale
	// BranchOrder entry must never resurrect a deleted branch.
	for _, b := range r.BranchOrder {
		if _, ok := r.Branches[b]; ok {
			branches = append(branches, b)
		}
	}
	// branches created via push may not be in BranchOrder; include them sorted
	seen := make(map[string]bool)
	for _, b := range branches {
		seen[b] = true
	}
	var extra []string
	for b := range r.Branches {
		if !seen[b] {
			extra = append(extra, b)
		}
	}
	sort.Strings(extra)
	branches = append(branches, extra...)
	for t := range r.Tags {
		tags = append(tags, t)
	}
	sort.Strings(tags)
	return branches, tags
}

func (s *Store) handleInfoRefs(w http.ResponseWriter, r *http.Request, repo *Repo, service string) {
	isV2 := r.Header.Get("Git-Protocol") == "version=2"
	if service == "git-upload-pack" && isV2 {
		w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
		w.WriteHeader(200)
		if repo.AdvV2 == nil {
			var b bytes.Buffer
			b.Write(pktLine("version 2\n"))
			b.Write(pktLine("agent=forge-go/1.0\n"))
			b.Write(pktLine("ls-refs=unborn\n"))
			b.Write(pktLine("fetch=shallow wait-for-done\n"))
			b.Write(pktLine("object-format=sha1\n"))
			b.Write(pktFlush)
			repo.AdvV2 = b.Bytes()
		}
		w.Write(repo.AdvV2)
		return
	}
	// v0 advertisement
	var ct string
	if service == "git-upload-pack" {
		ct = "application/x-git-upload-pack-advertisement"
	} else {
		ct = "application/x-git-receive-pack-advertisement"
	}
	w.Header().Set("Content-Type", ct)
	w.WriteHeader(200)
	var b bytes.Buffer
	b.Write(pktLine("# service="+service+"\n"))
	b.Write(pktFlush)
	branches, tags := s.allRefs(repo)
	var caps string
	if service == "git-upload-pack" {
		caps = "symref=HEAD:refs/heads/main agent=forge-go/1.0 object-format=sha1 side-band-64k thin-pack ofs-delta"
	} else {
		caps = "report-status report-status-v2 side-band-64k delete-refs ofs-delta agent=forge-go/1.0 object-format=sha1"
	}
	if len(branches) == 0 && len(tags) == 0 {
		b.Write(pktLine(strings.Repeat("0", 40) + " capabilities^{}\x00" + caps + "\n"))
		b.Write(pktFlush)
		w.Write(b.Bytes())
		return
	}
	first := true
	writeRef := func(oid OID, ref string) {
		line := oid.Hex() + " " + ref + "\n"
		if first {
			line = oid.Hex() + " " + ref + "\x00" + caps + "\n"
			first = false
		}
		b.Write(pktLine(line))
	}
	if tip, ok := repo.Branches["main"]; ok {
		writeRef(tip, "HEAD")
	}
	for _, br := range branches {
		writeRef(repo.Branches[br], "refs/heads/"+br)
	}
	for _, t := range tags {
		oid := repo.Tags[t]
		writeRef(oid, "refs/tags/"+t)
		if s.objectType(oid) == "tag" {
			b.Write(pktLine(s.resolveTag(oid).Hex() + " refs/tags/" + t + "^{}\n"))
		}
	}
	b.Write(pktFlush)
	w.Write(b.Bytes())
}

func (s *Store) handleUploadPack(w http.ResponseWriter, r *http.Request, repo *Repo, body []byte) {
	isV2 := r.Header.Get("Git-Protocol") == "version=2"
	if !isV2 {
		s.handleUploadPackV0(w, r, repo, body)
		return
	}
	lines := pktLines(body)
	var cmd string
	var wants, haves, shallowReq []OID
	maxDepth := 0
	deepen := false
	peel, symrefs, unborn := false, false, false
	var prefixes []string
	for _, ln := range lines {
		ln = strings.TrimSuffix(ln, "\n")
		switch {
		case strings.HasPrefix(ln, "command="):
			cmd = ln[len("command="):]
		case ln == "peel":
			peel = true
		case ln == "symrefs":
			symrefs = true
		case ln == "unborn":
			unborn = true
		case strings.HasPrefix(ln, "ref-prefix "):
			prefixes = append(prefixes, ln[len("ref-prefix "):])
		case strings.HasPrefix(ln, "want "):
			if o, ok := oidFromHex(strings.TrimSpace(ln[5:])); ok {
				wants = append(wants, o)
			}
		case strings.HasPrefix(ln, "have "):
			if o, ok := oidFromHex(strings.TrimSpace(ln[5:])); ok {
				haves = append(haves, o)
			}
		case strings.HasPrefix(ln, "shallow "):
			if o, ok := oidFromHex(strings.TrimSpace(ln[8:])); ok {
				shallowReq = append(shallowReq, o)
			}
		case strings.HasPrefix(ln, "deepen "):
			n, _ := strconv.Atoi(strings.TrimSpace(ln[7:]))
			maxDepth = n
			deepen = true
		}
	}
	w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
	w.WriteHeader(200)
	if cmd == "ls-refs" {
		s.writeLsRefs(w, repo, peel, symrefs, unborn, prefixes)
		return
	}
	// fetch
	s.mu.RLock()
	objs, shallow, unshallow := s.collectFetch(wants, haves, shallowReq, maxDepth, deepen)
	s.mu.RUnlock()
	var b bytes.Buffer
	b.Write(pktLine("acknowledgments\n"))
	b.Write(pktLine("NAK\n"))
	b.Write(pktDelim)
	if len(shallow) > 0 || len(unshallow) > 0 {
		b.Write(pktLine("shallow-info\n"))
		for _, o := range shallow {
			b.Write(pktLine("shallow " + o.Hex() + "\n"))
		}
		for _, o := range unshallow {
			b.Write(pktLine("unshallow " + o.Hex() + "\n"))
		}
		b.Write(pktDelim)
	}
	if len(wants) > 0 {
		b.Write(pktLine("packfile\n"))
		// memoize depth-1 single-want packs (bench clone)
		var pack []byte
		if maxDepth == 1 && len(wants) == 1 {
			key := "d1:" + wants[0].Hex()
			s.mu.RLock()
			pack = repo.PackCache[key]
			s.mu.RUnlock()
			if pack == nil {
				s.mu.RLock()
				pack = s.buildPack(objs)
				s.mu.RUnlock()
				s.mu.Lock()
				if repo.PackCache == nil {
					repo.PackCache = make(map[string][]byte)
				}
				repo.PackCache[key] = pack
				s.mu.Unlock()
			}
		} else {
			s.mu.RLock()
			pack = s.buildPack(objs)
			s.mu.RUnlock()
		}
		// sideband pack into the buffer via a writer that appends to b
		var sb bytes.Buffer
		writeSideband(&sb, 1, pack)
		b.Write(sb.Bytes())
	}
	b.Write(pktFlush)
	w.Write(b.Bytes())
}

func (s *Store) writeLsRefs(w http.ResponseWriter, repo *Repo, peel, symrefs, unborn bool, prefixes []string) {
	var b bytes.Buffer
	branches, tags := s.allRefs(repo)
	// multiple ref-prefix args are OR-ed; HEAD is a ref too, so it is only
	// reported when a prefix matches it (no prefixes = everything matches).
	// Reporting HEAD unconditionally broke prefix filtering: a refs/heads/
	// answer carried HEAD, and an unmatched prefix still returned 1 ref.
	match := func(ref string) bool {
		if len(prefixes) == 0 {
			return true
		}
		for _, p := range prefixes {
			if strings.HasPrefix(ref, p) {
				return true
			}
		}
		return false
	}
	if len(branches) == 0 && len(tags) == 0 {
		if unborn && match("HEAD") {
			b.Write(pktLine("unborn HEAD symref-target:refs/heads/main\n"))
		}
		b.Write(pktFlush)
		w.Write(b.Bytes())
		return
	}
	if match("HEAD") {
		if tip, ok := repo.Branches["main"]; ok {
			line := tip.Hex() + " HEAD"
			if symrefs {
				line += " symref-target:refs/heads/main"
			}
			b.Write(pktLine(line + "\n"))
		}
	}
	for _, br := range branches {
		ref := "refs/heads/" + br
		if !match(ref) {
			continue
		}
		b.Write(pktLine(repo.Branches[br].Hex() + " " + ref + "\n"))
	}
	for _, t := range tags {
		ref := "refs/tags/" + t
		if !match(ref) {
			continue
		}
		oid := repo.Tags[t]
		line := oid.Hex() + " " + ref
		if peel && s.objectType(oid) == "tag" {
			line += " peeled:" + s.resolveTag(oid).Hex()
		}
		b.Write(pktLine(line + "\n"))
	}
	b.Write(pktFlush)
	w.Write(b.Bytes())
}

func (s *Store) handleUploadPackV0(w http.ResponseWriter, r *http.Request, repo *Repo, body []byte) {
	lines := pktLines(body)
	var wants, haves, shallowReq []OID
	maxDepth := 0
	deepen := false
	for _, ln := range lines {
		ln = strings.TrimSuffix(ln, "\n")
		// first want line may carry caps after a space: "want <oid> <caps>"
		fields := strings.Fields(ln)
		if len(fields) == 0 {
			continue
		}
		switch fields[0] {
		case "want":
			if len(fields) >= 2 {
				if o, ok := oidFromHex(fields[1]); ok {
					wants = append(wants, o)
				}
			}
		case "have":
			if len(fields) >= 2 {
				if o, ok := oidFromHex(fields[1]); ok {
					haves = append(haves, o)
				}
			}
		case "shallow":
			if len(fields) >= 2 {
				if o, ok := oidFromHex(fields[1]); ok {
					shallowReq = append(shallowReq, o)
				}
			}
		case "deepen":
			if len(fields) >= 2 {
				n, _ := strconv.Atoi(fields[1])
				maxDepth = n
				deepen = true
			}
		}
	}
	s.mu.RLock()
	objs, shallow, unshallow := s.collectFetch(wants, haves, shallowReq, maxDepth, deepen)
	pack := s.buildPack(objs)
	s.mu.RUnlock()
	w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
	w.WriteHeader(200)
	var b bytes.Buffer
	b.Write(pktLine("NAK\n"))
	for _, o := range shallow {
		b.Write(pktLine("shallow " + o.Hex() + "\n"))
	}
	for _, o := range unshallow {
		b.Write(pktLine("unshallow " + o.Hex() + "\n"))
	}
	writeSideband(&b, 1, pack)
	b.Write(pktFlush)
	w.Write(b.Bytes())
}

type refCmd struct {
	old, new OID
	ref      string
}

func (s *Store) handleReceivePack(w http.ResponseWriter, r *http.Request, repo *Repo, body []byte) {
	cmdLines, packBytes := splitPackBody(body)
	var cmds []refCmd
	for _, ln := range cmdLines {
		ln = strings.TrimSuffix(ln, "\n")
		if i := strings.Index(ln, "\x00"); i >= 0 {
			ln = ln[:i]
		}
		f := strings.Fields(ln)
		if len(f) < 3 {
			continue
		}
		oldO, _ := oidFromHex(f[0])
		newO, _ := oidFromHex(f[1])
		cmds = append(cmds, refCmd{old: oldO, new: newO, ref: f[2]})
	}
	s.mu.Lock()
	s.packStarts = nil
	if len(packBytes) > 0 {
		s.readPack(packBytes)
	}
	s.packStarts = nil
	var report bytes.Buffer
	report.Write(pktLine("unpack ok\n"))
	for _, c := range cmds {
		ok, _ := s.applyRefLocked(repo, c)
		if ok {
			report.Write(pktLine("ok " + c.ref + "\n"))
		} else {
			report.Write(pktLine("ng " + c.ref + " non-fast-forward\n"))
		}
	}
	// The report is itself a pkt-line stream: it must end with a flush
	// INSIDE the band-1 payload (real git reads until that flush; the
	// outer flush after the sideband packets is a different terminator).
	report.Write(pktFlush)
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/x-git-receive-pack-result")
	w.WriteHeader(200)
	var b bytes.Buffer
	writeSideband(&b, 1, report.Bytes())
	b.Write(pktFlush)
	w.Write(b.Bytes())
}

func (s *Store) applyRefLocked(repo *Repo, c refCmd) (bool, string) {
	isBranch := strings.HasPrefix(c.ref, "refs/heads/")
	isTag := strings.HasPrefix(c.ref, "refs/tags/")
	if !isBranch && !isTag {
		return false, "bad ref"
	}
	name := strings.TrimPrefix(strings.TrimPrefix(c.ref, "refs/heads/"), "refs/tags/")
	var cur OID
	var exists bool
	if isBranch {
		cur, exists = repo.Branches[name]
	} else {
		cur, exists = repo.Tags[name]
	}
	if c.new.IsZero() {
		// delete
		if !exists || cur != c.old {
			return false, "delete mismatch"
		}
		if isBranch {
			delete(repo.Branches, name)
			// BranchOrder is the advertisement source: a deleted branch
			// must leave it too, or ls-refs keeps advertising the ref.
			kept := repo.BranchOrder[:0]
			for _, b := range repo.BranchOrder {
				if b != name {
					kept = append(kept, b)
				}
			}
			repo.BranchOrder = kept
		} else {
			delete(repo.Tags, name)
		}
		s.invalidate(repo)
		return true, ""
	}
	// new must be an object we hold
	if _, ok := s.Objects[c.new]; !ok {
		return false, "missing object"
	}
	if c.old.IsZero() {
		// create
		if exists {
			return false, "already exists"
		}
	} else {
		if !exists || cur != c.old {
			return false, "old mismatch"
		}
		if isBranch && !s.isDescendant(c.new, c.old) {
			return false, "non-fast-forward"
		}
	}
	if isBranch {
		repo.Branches[name] = c.new
		found := false
		for _, b := range repo.BranchOrder {
			if b == name {
				found = true
			}
		}
		if !found {
			repo.BranchOrder = append(repo.BranchOrder, name)
		}
	} else {
		repo.Tags[name] = c.new
	}
	s.invalidate(repo)
	return true, ""
}
