// Git objects + packfile: SHA-1, parse commit/tree/tag, baca pack v2/v3
// (termasuk OFS_DELTA dan REF_DELTA), tulis pack polos (tanpa delta —
// pack-deltas lolos bila server tidak mengirim delta), traversal reachable
// dengan batas depth, dan cek fast-forward.
package main

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/binary"
	"errors"
	"io"
	"sort"
	"strconv"
)

// ---------------------------------------------------------------------------
// Parse commit / tree / tag
// ---------------------------------------------------------------------------

type commitInfo struct {
	Tree      string
	Parents   []string
	Author    string
	Committer string
	Message   string
}

func parseCommit(data []byte) commitInfo {
	var c commitInfo
	// header berakhir di baris kosong pertama
	idx := bytes.Index(data, []byte("\n\n"))
	head := data
	if idx >= 0 {
		head = data[:idx]
		c.Message = string(data[idx+2:])
	}
	for _, line := range bytes.Split(head, []byte("\n")) {
		s := string(line)
		switch {
		case len(s) > 5 && s[:5] == "tree ":
			c.Tree = s[5:]
		case len(s) > 7 && s[:7] == "parent ":
			c.Parents = append(c.Parents, s[7:])
		case len(s) > 7 && s[:7] == "author ":
			c.Author = s[7:]
		case len(s) > 10 && s[:10] == "committer ":
			c.Committer = s[10:]
		}
	}
	return c
}

type treeEntry struct {
	Mode string
	Name string
	OID  string
}

func parseTree(data []byte) []treeEntry {
	var out []treeEntry
	for len(data) > 0 {
		sp := bytes.IndexByte(data, ' ')
		if sp < 0 {
			break
		}
		mode := string(data[:sp])
		rest := data[sp+1:]
		nul := bytes.IndexByte(rest, 0)
		if nul < 0 || len(rest) < nul+21 {
			break
		}
		name := string(rest[:nul])
		var o oid
		copy(o[:], rest[nul+1:nul+21])
		out = append(out, treeEntry{Mode: mode, Name: name, OID: o.hex()})
		data = rest[nul+21:]
	}
	return out
}

type tagInfo struct {
	Object string
	Type   string
	Tag    string
	Tagger string
}

func parseTag(data []byte) tagInfo {
	var t tagInfo
	idx := bytes.Index(data, []byte("\n\n"))
	head := data
	if idx >= 0 {
		head = data[:idx]
	}
	for _, line := range bytes.Split(head, []byte("\n")) {
		s := string(line)
		switch {
		case len(s) > 7 && s[:7] == "object ":
			t.Object = s[7:]
		case len(s) > 5 && s[:5] == "type ":
			t.Type = s[5:]
		case len(s) > 4 && s[:4] == "tag ":
			t.Tag = s[4:]
		case len(s) > 7 && s[:7] == "tagger ":
			t.Tagger = s[7:]
		}
	}
	return t
}

// buildTree membuat objek tree dari entri (mode, nama, oid hex); entri
// diurutkan seperti git (nama direktori diurutkan seolah berakhiran '/').
func buildTree(entries []treeEntry) (oid, []byte) {
	sorted := append([]treeEntry(nil), entries...)
	sort.Slice(sorted, func(i, j int) bool {
		ni, nj := sorted[i].Name, sorted[j].Name
		if sorted[i].Mode == "40000" {
			ni += "/"
		}
		if sorted[j].Mode == "40000" {
			nj += "/"
		}
		return ni < nj
	})
	var buf bytes.Buffer
	for _, e := range sorted {
		buf.WriteString(e.Mode)
		buf.WriteByte(' ')
		buf.WriteString(e.Name)
		buf.WriteByte(0)
		o, _ := oidFromHex(e.OID)
		buf.Write(o[:])
	}
	data := buf.Bytes()
	return hashObject("tree", data), data
}

// ---------------------------------------------------------------------------
// Packfile: tulis (polos) dan baca (dengan delta)
// ---------------------------------------------------------------------------

func writePack(objs map[oid]*gitObject, order []oid) []byte {
	var buf bytes.Buffer
	buf.WriteString("PACK")
	_ = binary.Write(&buf, binary.BigEndian, uint32(2))
	_ = binary.Write(&buf, binary.BigEndian, uint32(len(order)))
	for _, id := range order {
		o := objs[id]
		if o == nil {
			continue
		}
		var typeNum byte
		switch o.typ {
		case "commit":
			typeNum = 1
		case "tree":
			typeNum = 2
		case "blob":
			typeNum = 3
		case "tag":
			typeNum = 4
		}
		// header varint: type di bit 4-6, ukuran di bit 0-3 + kelanjutan
		size := len(o.data)
		first := byte(size&0x0f) | (typeNum << 4)
		size >>= 4
		if size > 0 {
			first |= 0x80
		}
		buf.WriteByte(first)
		for size > 0 {
			b := byte(size & 0x7f)
			size >>= 7
			if size > 0 {
				b |= 0x80
			}
			buf.WriteByte(b)
		}
		zw := zlib.NewWriter(&buf)
		_, _ = zw.Write(o.data)
		_ = zw.Close()
	}
	sum := sha1.Sum(buf.Bytes())
	buf.Write(sum[:])
	return buf.Bytes()
}

func verifyPackChecksum(pack []byte) bool {
	if len(pack) < 20 {
		return false
	}
	sum := sha1.Sum(pack[:len(pack)-20])
	return bytes.Equal(sum[:], pack[len(pack)-20:])
}

type packEntry struct {
	typ     string
	data    []byte
	base    oid // untuk delta: oid base (REF_DELTA) atau kosong (OFS)
	ofsBase int64
	isDelta bool
}

func applyDelta(base, delta []byte) ([]byte, error) {
	if len(delta) < 2 {
		return nil, errors.New("delta too short")
	}
	// varint: ukuran source, lalu ukuran target
	readVarint := func(b []byte, pos int) (int, int, error) {
		v := 0
		shift := 0
		for {
			if pos >= len(b) {
				return 0, 0, errors.New("delta varint truncated")
			}
			c := b[pos]
			pos++
			v |= int(c&0x7f) << shift
			shift += 7
			if c&0x80 == 0 {
				break
			}
		}
		return v, pos, nil
	}
	srcSize, pos, err := readVarint(delta, 0)
	if err != nil {
		return nil, err
	}
	if srcSize != len(base) {
		return nil, errors.New("delta base size mismatch")
	}
	_, pos, err = readVarint(delta, pos)
	if err != nil {
		return nil, err
	}
	var out []byte
	for pos < len(delta) {
		op := delta[pos]
		pos++
		if op&0x80 != 0 {
			// copy dari base: offset/size per bit
			off, size := 0, 0
			for i := 0; i < 4; i++ {
				if op&(1<<uint(i)) != 0 {
					if pos >= len(delta) {
						return nil, errors.New("delta copy truncated")
					}
					off |= int(delta[pos]) << (8 * uint(i))
					pos++
				}
			}
			for i := 0; i < 3; i++ {
				if op&(0x10<<uint(i)) != 0 {
					if pos >= len(delta) {
						return nil, errors.New("delta copy truncated")
					}
					size |= int(delta[pos]) << (8 * uint(i))
					pos++
				}
			}
			if size == 0 {
				size = 0x10000
			}
			if off+size > len(base) {
				return nil, errors.New("delta copy out of range")
			}
			out = append(out, base[off:off+size]...)
		} else if op != 0 {
			if pos+int(op) > len(delta) {
				return nil, errors.New("delta insert truncated")
			}
			out = append(out, delta[pos:pos+int(op)]...)
			pos += int(op)
		} else {
			return nil, errors.New("delta opcode 0")
		}
	}
	return out, nil
}

// readPack mengurai packfile dan mengembalikan objek yang sudah di-resolve
// (delta diterapkan). have menyediakan objek base yang sudah dimiliki klien
// (untuk thin pack REF_DELTA ke base di luar pack).
func readPack(pack []byte, have map[string]*gitObject) (map[oid]*gitObject, error) {
	out := map[oid]*gitObject{}
	if len(pack) < 12 || string(pack[:4]) != "PACK" {
		return nil, errors.New("not a packfile")
	}
	version := binary.BigEndian.Uint32(pack[4:8])
	if version != 2 && version != 3 {
		return nil, errors.New("unsupported pack version")
	}
	count := int(binary.BigEndian.Uint32(pack[8:12]))
	pos := 12
	type rawEntry struct {
		id      oid
		typ     string
		data    []byte
		baseOID oid
		baseOff int64
		delta   bool
	}
	entries := make([]rawEntry, 0, count)
	offsets := make([]int64, 0, count)
	for i := 0; i < count; i++ {
		start := int64(pos)
		offsets = append(offsets, start)
		if pos >= len(pack) {
			return nil, errors.New("pack truncated")
		}
		b := pack[pos]
		pos++
		typeNum := (b >> 4) & 0x07
		size := int64(b & 0x0f)
		shift := uint(4)
		for b&0x80 != 0 {
			if pos >= len(pack) {
				return nil, errors.New("pack header truncated")
			}
			b = pack[pos]
			pos++
			size |= int64(b&0x7f) << shift
			shift += 7
		}
		var typ string
		var baseOID oid
		var baseOff int64
		isDelta := false
		switch typeNum {
		case 1:
			typ = "commit"
		case 2:
			typ = "tree"
		case 3:
			typ = "blob"
		case 4:
			typ = "tag"
		case 6: // OFS_DELTA
			isDelta = true
			// offset varint (git encoding)
			if pos >= len(pack) {
				return nil, errors.New("ofs delta truncated")
			}
			c := pack[pos]
			pos++
			off := int64(c & 0x7f)
			for c&0x80 != 0 {
				if pos >= len(pack) {
					return nil, errors.New("ofs delta truncated")
				}
				c = pack[pos]
				pos++
				off = ((off + 1) << 7) | int64(c&0x7f)
			}
			baseOff = start - off
		case 7: // REF_DELTA
			isDelta = true
			if pos+20 > len(pack) {
				return nil, errors.New("ref delta truncated")
			}
			copy(baseOID[:], pack[pos:pos+20])
			pos += 20
		default:
			return nil, errors.New("unknown pack object type")
		}
		// Inflate SEKALI dan hitung byte terkompresi dari posisi reader
		// (bytes.Reader = io.ByteReader, jadi flate tidak over-read —
		// diverifikasi empiris di /tmp/ztest untuk ukuran 0–200 KB).
		// Sebelumnya: zlibConsumed melakukan binary search dengan
		// inflate ulang ~log2(n) kali PER OBJEK — itulah bottleneck push.
		br := bytes.NewReader(pack[pos:])
		zr, err := zlib.NewReader(br)
		if err != nil {
			return nil, err
		}
		data, err := io.ReadAll(zr)
		_ = zr.Close()
		if err != nil {
			return nil, err
		}
		compLen := len(pack[pos:]) - br.Len()
		pos += compLen
		if isDelta {
			entries = append(entries, rawEntry{typ: "delta", data: data, baseOID: baseOID, baseOff: baseOff, delta: true})
			// id sementara: diisi saat resolve (hash dari hasil delta)
			_ = size
		} else {
			id := hashObject(typ, data)
			entries = append(entries, rawEntry{id: id, typ: typ, data: data})
			out[id] = &gitObject{typ: typ, data: data}
		}
		_ = start
	}
	// resolve delta: base bisa dari pack (by offset) atau dari have.
	byOffset := map[int64]oid{}
	for i, e := range entries {
		if !e.delta {
			byOffset[offsets[i]] = e.id
		}
	}
	// beberapa putaran sampai semua delta ter-resolve
	for round := 0; round < len(entries)+2; round++ {
		progress := false
		for i, e := range entries {
			if !e.delta || e.id != (oid{}) {
				continue
			}
			var base *gitObject
			if e.baseOff != 0 {
				if bid, ok := byOffset[e.baseOff]; ok {
					base = out[bid]
				}
			} else {
				if b, ok := out[e.baseOID]; ok {
					base = b
				} else if have != nil {
					if b, ok := have[e.baseOID.hex()]; ok {
						base = b
					}
				}
			}
			if base == nil {
				continue
			}
			resolved, err := applyDelta(base.data, e.data)
			if err != nil {
				return nil, err
			}
			id := hashObject(base.typ, resolved)
			entries[i].id = id
			entries[i].typ = base.typ
			out[id] = &gitObject{typ: base.typ, data: resolved}
			byOffset[offsets[i]] = id
			progress = true
		}
		if !progress {
			break
		}
	}
	for _, e := range entries {
		if e.delta && e.id == (oid{}) {
			return nil, errors.New("unresolved delta in pack")
		}
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Traversal reachable
// ---------------------------------------------------------------------------

// reachableFrom mengembalikan semua objek yang terjangkau dari oids awal.
// depth < 0 berarti tanpa batas; depth >= 0 membatasi generasi commit
// (0 = commit awal saja). shallowOut diisi commit yang menjadi batas
// (commit yang parent-nya tidak ikut karena depth).
func (r *repo) reachableFrom(starts []oid, depth int, haveStop map[oid]bool) (map[oid]*gitObject, []oid) {
	included := map[oid]*gitObject{}
	var shallow []oid
	// BFS per commit dengan jarak
	type item struct {
		id    oid
		dist  int
		isTag bool
	}
	queue := []item{}
	seenCommit := map[oid]bool{}
	for _, s := range starts {
		queue = append(queue, item{id: s, dist: 0})
	}
	for len(queue) > 0 {
		it := queue[0]
		queue = queue[1:]
		if _, ok := included[it.id]; ok {
			continue
		}
		o := r.Objects[it.id]
		if o == nil {
			continue
		}
		switch o.typ {
		case "commit":
			if seenCommit[it.id] {
				continue
			}
			seenCommit[it.id] = true
			// Depth: commit pada jarak >= depth TIDAK dikirim sama sekali
			// (depth 1 = hanya tip + tree/blob-nya; depth 2 = tip + parent).
			// Sebelumnya boundary commit IKUT terkirim — itulah sebab
			// fetch-shallow-depth1 gagal (parent hadir padahal harus absen).
			if depth >= 0 && it.dist >= depth {
				continue
			}
			included[it.id] = o
			ci := parseCommit(o.data)
			if treeOID, ok := oidFromHex(ci.Tree); ok {
				r.addTreeClosure(included, treeOID)
			}
			// Boundary terdalam yang dikirim dinamai shallow.
			if depth >= 0 && it.dist == depth-1 {
				shallow = append(shallow, it.id)
			}
			// haveStop: commit ini batas shallow milik klien — kirim
			// commit + tree ini, tapi JANGAN lewati ke parent-nya.
			// Sebelumnya cek dilakukan pada parent (terbalik), sehingga
			// deepen mengirim 0 objek.
			if haveStop != nil && haveStop[it.id] {
				continue
			}
			for _, p := range ci.Parents {
				if poid, ok := oidFromHex(p); ok {
					queue = append(queue, item{id: poid, dist: it.dist + 1})
				}
			}
		case "tag":
			included[it.id] = o
			ti := parseTag(o.data)
			if toid, ok := oidFromHex(ti.Object); ok {
				queue = append(queue, item{id: toid, dist: it.dist})
			}
		case "tree":
			included[it.id] = o
			r.addTreeClosure(included, it.id)
		default:
			included[it.id] = o
		}
	}
	return included, shallow
}

// jarakGenerasi mengembalikan jarak commit target dari starts (0 = start
// itu sendiri), atau false bila target bukan ancestor dari starts.
// Dipakai deepen-relative: kedalaman boundary lama dari wants.
func (r *repo) jarakGenerasi(starts []oid, target oid) (int, bool) {
	type item struct {
		id   oid
		dist int
	}
	queue := []item{}
	seen := map[oid]bool{}
	for _, s := range starts {
		if !seen[s] {
			seen[s] = true
			queue = append(queue, item{s, 0})
		}
	}
	for len(queue) > 0 {
		it := queue[0]
		queue = queue[1:]
		if it.id == target {
			return it.dist, true
		}
		o := r.Objects[it.id]
		if o == nil || o.typ != "commit" {
			continue
		}
		ci := parseCommit(o.data)
		for _, p := range ci.Parents {
			if poid, ok := oidFromHex(p); ok && !seen[poid] {
				seen[poid] = true
				queue = append(queue, item{poid, it.dist + 1})
			}
		}
	}
	return 0, false
}

// addTreeClosure menambahkan tree + subtree + blob yang dirujuk.
func (r *repo) addTreeClosure(included map[oid]*gitObject, treeID oid) {
	if _, ok := included[treeID]; ok {
		return
	}
	o := r.Objects[treeID]
	if o == nil || o.typ != "tree" {
		return
	}
	included[treeID] = o
	for _, e := range parseTree(o.data) {
		eoid, ok := oidFromHex(e.OID)
		if !ok {
			continue
		}
		if e.Mode == "40000" {
			r.addTreeClosure(included, eoid)
		} else if bo := r.Objects[eoid]; bo != nil {
			included[eoid] = bo
		}
	}
}

// isAncestor: apakah anc adalah ancestor (atau sama) dari desc.
func (r *repo) isAncestor(anc, desc oid) bool {
	if anc == desc {
		return true
	}
	seen := map[oid]bool{}
	queue := []oid{desc}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if seen[cur] {
			continue
		}
		seen[cur] = true
		o := r.Objects[cur]
		if o == nil || o.typ != "commit" {
			continue
		}
		for _, p := range parseCommit(o.data).Parents {
			poid, ok := oidFromHex(p)
			if !ok {
				continue
			}
			if poid == anc {
				return true
			}
			queue = append(queue, poid)
		}
	}
	return false
}

// flattenTree mengembalikan map path -> blob oid untuk commit tertentu.
func (r *repo) flattenTree(treeID oid, prefix string, out map[string]string) {
	o := r.Objects[treeID]
	if o == nil || o.typ != "tree" {
		return
	}
	for _, e := range parseTree(o.data) {
		path := e.Name
		if prefix != "" {
			path = prefix + "/" + e.Name
		}
		eoid, ok := oidFromHex(e.OID)
		if !ok {
			continue
		}
		if e.Mode == "40000" {
			r.flattenTree(eoid, path, out)
		} else {
			out[path] = e.OID
		}
	}
}

// treeOfCommit mengembalikan map path->blob untuk tip commit.
func (r *repo) treeOfCommit(commitID oid) map[string]string {
	out := map[string]string{}
	o := r.Objects[commitID]
	if o == nil || o.typ != "commit" {
		return out
	}
	if treeOID, ok := oidFromHex(parseCommit(o.data).Tree); ok {
		r.flattenTree(treeOID, "", out)
	}
	return out
}

// buildTreeFromPaths membangun tree bertingkat dari map path->blob oid.
func (r *repo) buildTreeFromPaths(files map[string]string) oid {
	// bangun secara rekursif per direktori
	var build func(prefix string, paths map[string]string) oid
	build = func(prefix string, paths map[string]string) oid {
		dirs := map[string]map[string]string{}
		var entries []treeEntry
		for p, blobOID := range paths {
			rel := p
			if prefix != "" {
				rel = p[len(prefix)+1:]
			}
			if idx := indexByte(rel, '/'); idx >= 0 {
				dir := rel[:idx]
				if dirs[dir] == nil {
					dirs[dir] = map[string]string{}
				}
				dirs[dir][p] = blobOID
			} else {
				entries = append(entries, treeEntry{Mode: "100644", Name: rel, OID: blobOID})
			}
		}
		for dir, sub := range dirs {
			subOID := build(joinPath(prefix, dir), sub)
			entries = append(entries, treeEntry{Mode: "40000", Name: dir, OID: subOID.hex()})
		}
		id, data := buildTree(entries)
		r.Objects[id] = &gitObject{typ: "tree", data: data}
		return id
	}
	return build("", files)
}

func indexByte(s string, c byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == c {
			return i
		}
	}
	return -1
}

func joinPath(a, b string) string {
	if a == "" {
		return b
	}
	return a + "/" + b
}

// sortedOIDs mengurutkan oid secara deterministik untuk pack.
func sortedOIDs(m map[oid]*gitObject) []oid {
	out := make([]oid, 0, len(m))
	for id := range m {
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].hex() < out[j].hex() })
	return out
}

var _ = strconv.Itoa
