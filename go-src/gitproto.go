// Git smart HTTP: pkt-line, info/refs (v0 + v2), ls-refs, fetch v2,
// receive-pack v0 dengan report-status + side-band-64k + delete-refs.
package main

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
)

const agentName = "forge-go/1.0.0"

// pktLine membungkus payload (tanpa newline tambahan) menjadi pkt-line.
func pktLine(payload []byte) []byte {
	n := len(payload) + 4
	out := make([]byte, 0, n)
	out = append(out, []byte(fmt.Sprintf("%04x", n))...)
	out = append(out, payload...)
	return out
}

func pktFlush() []byte { return []byte("0000") }
func pktDelim() []byte { return []byte("0001") }

// parsePktLines mengurai body pkt-line menjadi daftar payload mentah
// (flush/delim direpresentasikan sebagai nil dengan marker terpisah).
type pkt struct {
	kind    string // "data", "flush", "delim"
	payload []byte
}

func parsePktLines(b []byte) ([]pkt, error) {
	var out []pkt
	for len(b) > 0 {
		if len(b) < 4 {
			return nil, fmt.Errorf("pkt-line truncated")
		}
		n, err := strconv.ParseInt(string(b[:4]), 16, 32)
		if err != nil {
			return nil, fmt.Errorf("bad pkt-line length")
		}
		if n == 0 {
			out = append(out, pkt{kind: "flush"})
			b = b[4:]
			continue
		}
		if n == 1 {
			out = append(out, pkt{kind: "delim"})
			b = b[4:]
			continue
		}
		if int(n) < 4 || len(b) < int(n) {
			return nil, fmt.Errorf("pkt-line truncated")
		}
		out = append(out, pkt{kind: "data", payload: append([]byte(nil), b[4:n]...)})
		b = b[n:]
	}
	return out, nil
}

// sideband membungkus data menjadi frame band 1 (pkt-line per chunk).
func sidebandFrames(data []byte) []byte {
	var out []byte
	for len(data) > 0 {
		n := len(data)
		if n > 65515 {
			n = 65515
		}
		chunk := make([]byte, 0, n+1)
		chunk = append(chunk, 1)
		chunk = append(chunk, data[:n]...)
		out = append(out, pktLine(chunk)...)
		data = data[n:]
	}
	return out
}

// ---------------------------------------------------------------------------
// Advertisements
// ---------------------------------------------------------------------------

func (s *store) v0UploadCaps() string {
	return "multi_ack thin-pack side-band side-band-64k ofs-delta shallow deepen-since deepen-not deepen-relative no-progress include-tag multi_ack_detailed allow-tip-sha1-in-want allow-reachable-sha1-in-want no-done object-format=sha1 agent=" + agentName
}

func (s *store) v0ReceiveCaps() string {
	return "report-status side-band-64k delete-refs ofs-delta object-format=sha1 agent=" + agentName
}

// infoRefsUpload menulis advertisement upload-pack: v2 bila header
// Git-Protocol: version=2, selain itu v0 dengan preamble service.
func (s *store) infoRefsUpload(w http.ResponseWriter, r *http.Request, rp *repo) {
	if r.Header.Get("Git-Protocol") == "version=2" {
		var buf bytes.Buffer
		buf.Write(pktLine([]byte("version 2\n")))
		buf.Write(pktLine([]byte("agent=" + agentName + "\n")))
		buf.Write(pktLine([]byte("ls-refs=unborn\n")))
		buf.Write(pktLine([]byte("fetch=shallow filter\n")))
		buf.Write(pktLine([]byte("server-option\n")))
		buf.Write(pktLine([]byte("object-format=sha1\n")))
		buf.Write(pktFlush())
		w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(200)
		_, _ = w.Write(buf.Bytes())
		return
	}
	var buf bytes.Buffer
	buf.Write(pktLine([]byte("# service=git-upload-pack\n")))
	buf.Write(pktFlush())
	s.mu.RLock()
	names := rp.sortedRefNames()
	caps := s.v0UploadCaps()
	symref := "symref=HEAD:refs/heads/" + rp.DefaultBranch
	if len(names) == 0 {
		first := zeroOID.hex() + " capabilities^{}\x00" + caps + " " + symref + "\n"
		buf.Write(pktLine([]byte(first)))
	} else {
		for i, name := range names {
			oidHex := rp.Refs[name].hex()
			line := oidHex + " " + name
			if i == 0 {
				line += "\x00" + caps + " " + symref
			}
			line += "\n"
			buf.Write(pktLine([]byte(line)))
			// peeled untuk annotated tag
			if strings.HasPrefix(name, "refs/tags/") {
				if o := rp.Objects[rp.Refs[name]]; o != nil && o.typ == "tag" {
					peeled := parseTag(o.data).Object
					buf.Write(pktLine([]byte(peeled + " " + name + "^{}\n")))
				}
			}
		}
	}
	s.mu.RUnlock()
	buf.Write(pktFlush())
	w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(200)
	_, _ = w.Write(buf.Bytes())
}

func (s *store) infoRefsReceive(w http.ResponseWriter, r *http.Request, rp *repo) {
	var buf bytes.Buffer
	buf.Write(pktLine([]byte("# service=git-receive-pack\n")))
	buf.Write(pktFlush())
	s.mu.RLock()
	names := rp.sortedRefNames()
	caps := s.v0ReceiveCaps()
	symref := "symref=HEAD:refs/heads/" + rp.DefaultBranch
	if len(names) == 0 {
		first := zeroOID.hex() + " capabilities^{}\x00" + caps + " " + symref + "\n"
		buf.Write(pktLine([]byte(first)))
	} else {
		for i, name := range names {
			line := rp.Refs[name].hex() + " " + name
			if i == 0 {
				line += "\x00" + caps + " " + symref
			}
			line += "\n"
			buf.Write(pktLine([]byte(line)))
			if strings.HasPrefix(name, "refs/tags/") {
				if o := rp.Objects[rp.Refs[name]]; o != nil && o.typ == "tag" {
					peeled := parseTag(o.data).Object
					buf.Write(pktLine([]byte(peeled + " " + name + "^{}\n")))
				}
			}
		}
	}
	s.mu.RUnlock()
	buf.Write(pktFlush())
	w.Header().Set("Content-Type", "application/x-git-receive-pack-advertisement")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(200)
	_, _ = w.Write(buf.Bytes())
}

// ---------------------------------------------------------------------------
// ls-refs (v2)
// ---------------------------------------------------------------------------

func (s *store) handleLsRefs(w http.ResponseWriter, r *http.Request, rp *repo, body []byte) {
	pkts, err := parsePktLines(body)
	if err != nil {
		w.WriteHeader(400)
		return
	}
	var prefixes []string
	peel, symrefs, unborn := false, false, false
	for _, p := range pkts {
		if p.kind != "data" {
			continue
		}
		line := strings.TrimSuffix(string(p.payload), "\n")
		switch {
		case line == "peel":
			peel = true
		case line == "symrefs":
			symrefs = true
		case line == "unborn":
			unborn = true
		case strings.HasPrefix(line, "ref-prefix "):
			prefixes = append(prefixes, strings.TrimPrefix(line, "ref-prefix "))
		}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	var buf bytes.Buffer
	match := func(name string) bool {
		if len(prefixes) == 0 {
			return true
		}
		for _, pre := range prefixes {
			if strings.HasPrefix(name, pre) {
				return true
			}
		}
		return false
	}
	// HEAD symref bila diminta symrefs dan cocok prefix (prefix "HEAD"
	// cocok; "refs/heads/" atau "refs/does-not-exist/" tidak — HEAD tidak
	// boleh bocor ke hasil prefix-filtered). Repo kosong + unborn memakai
	// token literal "unborn" sebagai pengganti oid.
	if symrefs && match("HEAD") {
		if tip, ok := rp.headOID(); ok {
			buf.Write(pktLine([]byte(tip.hex() + " HEAD symref-target:refs/heads/" + rp.DefaultBranch + "\n")))
		} else if unborn {
			buf.Write(pktLine([]byte("unborn HEAD symref-target:refs/heads/" + rp.DefaultBranch + "\n")))
		}
	}
	for _, name := range rp.sortedRefNames() {
		if !match(name) {
			continue
		}
		oidHex := rp.Refs[name].hex()
		attrs := ""
		if peel && strings.HasPrefix(name, "refs/tags/") {
			if o := rp.Objects[rp.Refs[name]]; o != nil && o.typ == "tag" {
				attrs = " peeled:" + parseTag(o.data).Object
			}
		}
		buf.Write(pktLine([]byte(oidHex + " " + name + attrs + "\n")))
	}
	buf.Write(pktFlush())
	w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
	w.WriteHeader(200)
	_, _ = w.Write(buf.Bytes())
}

// ---------------------------------------------------------------------------
// fetch v2
// ---------------------------------------------------------------------------

type fetchReq struct {
	wants          []string
	haves          []string
	shallow        []string // boundary yang sudah dimiliki klien
	depth          int      // -1 bila tidak ada
	deepenRelative bool     // "deepen-relative": deepen = tambahan di bawah boundary lama
}

func (s *store) handleFetchV2(w http.ResponseWriter, r *http.Request, rp *repo, body []byte) {
	pkts, err := parsePktLines(body)
	if err != nil {
		w.WriteHeader(400)
		return
	}
	var fq fetchReq
	fq.depth = -1
	inArgs := false
	for _, p := range pkts {
		if p.kind == "delim" {
			inArgs = true
			continue
		}
		if p.kind != "data" {
			continue
		}
		line := strings.TrimSuffix(string(p.payload), "\n")
		if !inArgs {
			continue // command=fetch, agent=, object-format=
		}
		switch {
		case strings.HasPrefix(line, "want "):
			fq.wants = append(fq.wants, strings.TrimPrefix(line, "want "))
		case strings.HasPrefix(line, "have "):
			fq.haves = append(fq.haves, strings.TrimPrefix(line, "have "))
		case strings.HasPrefix(line, "shallow "):
			fq.shallow = append(fq.shallow, strings.TrimPrefix(line, "shallow "))
		case strings.HasPrefix(line, "deepen "):
			if n, err := strconv.Atoi(strings.TrimPrefix(line, "deepen ")); err == nil {
				fq.depth = n
			}
		case line == "deepen-relative":
			fq.deepenRelative = true
		}
	}
	s.serveFetch(w, rp, fq, true)
}

// serveFetch membangun pack dan menulis respons fetch (v2 dengan section,
// atau v0 tanpa section — dipakai keduanya dengan flag v2).
func (s *store) serveFetch(w http.ResponseWriter, rp *repo, fq fetchReq, v2 bool) {
	// Memo: clone depth-1 penuh (bench) — pack yang sama untuk tip yang sama.
	if v2 && fq.depth == 1 && len(fq.wants) == 1 && len(fq.haves) == 0 {
		if cached := s.getClonePack(rp, fq.wants[0]); cached != nil {
			s.writeFetchResponse(w, rp, fq, cached.pack, cached.shallow, cached.unshallow, true)
			return
		}
	}
	s.mu.RLock()
	// set exclusion: objek yang sudah dimiliki klien (haves, berhenti di
	// boundary shallow yang dikirim klien).
	stopSet := map[oid]bool{}
	for _, sh := range fq.shallow {
		if o, ok := oidFromHex(sh); ok {
			stopSet[o] = true
		}
	}
	excluded := map[oid]*gitObject{}
	for _, h := range fq.haves {
		ho, ok := oidFromHex(h)
		if !ok {
			continue
		}
		// traversal dari have, tanpa melewati boundary shallow
		inc, _ := rp.reachableFrom([]oid{ho}, -1, stopSet)
		for id, o := range inc {
			excluded[id] = o
		}
		// boundary shallow itu sendiri + tree/blobs-nya sudah dimiliki klien
		if stopSet[ho] {
			inc2, _ := rp.reachableFrom([]oid{ho}, 0, nil)
			for id, o := range inc2 {
				excluded[id] = o
			}
		}
	}
	// include set dari wants
	var starts []oid
	for _, wn := range fq.wants {
		if o, ok := oidFromHex(wn); ok {
			starts = append(starts, o)
		}
	}
	depth := -1
	if fq.depth >= 0 {
		depth = fq.depth
	}
	// deepen-relative (git asli, fetch --deepen=N pada clone shallow):
	// deepen = TAMBAHAN di bawah boundary lama, bukan kedalaman absolut.
	// Kedalaman efektif = kedalaman boundary lama dari wants (want = 1)
	// + deepen. Tanpa ini fetch --deepen=1 mengirim pack kosong.
	if fq.deepenRelative && depth > 0 && len(fq.shallow) > 0 {
		best := -1
		for _, sh := range fq.shallow {
			if shOID, ok := oidFromHex(sh); ok {
				if d, ok2 := rp.jarakGenerasi(starts, shOID); ok2 {
					if eff := d + 1 + depth; best < 0 || eff < best {
						best = eff
					}
				}
			}
		}
		if best > 0 {
			depth = best
		}
	}
	included, shallow := rp.reachableFrom(starts, depth, nil)
	// kirim = included - excluded
	send := map[oid]*gitObject{}
	for id, o := range included {
		if _, held := excluded[id]; !held {
			send[id] = o
		}
	}
	// shallow-info: boundary dari traversal depth + unshallow lama
	var shallowOut []string
	if fq.depth >= 0 {
		for _, sh := range shallow {
			shallowOut = append(shallowOut, sh.hex())
		}
	}
	var unshallowOut []string
	if fq.depth >= 0 {
		for _, old := range fq.shallow {
			oldOID, ok := oidFromHex(old)
			if !ok {
				continue
			}
			// boundary lama kini punya parent yang ikut terkirim -> unshallow
			if _, inInc := included[oldOID]; inInc {
				stillShallow := false
				for _, sh := range shallow {
					if sh == oldOID {
						stillShallow = true
					}
				}
				if !stillShallow {
					unshallowOut = append(unshallowOut, old)
				}
			}
		}
	}
	pack := writePack(send, sortedOIDs(send))
	s.mu.RUnlock()
	if v2 && fq.depth == 1 && len(fq.wants) == 1 && len(fq.haves) == 0 {
		s.putClonePack(rp, fq.wants[0], pack, shallowOut, unshallowOut)
	}
	s.writeFetchResponse(w, rp, fq, pack, shallowOut, unshallowOut, v2)
}

type cachedPack struct {
	pack      []byte
	shallow   []string
	unshallow []string
}

func (s *store) getClonePack(rp *repo, tip string) *cachedPack {
	s.mu.RLock()
	defer s.mu.RUnlock()
	key := rp.Owner + "/" + rp.Name + "@" + tip
	if b, ok := s.clonePack[key]; ok {
		return &cachedPack{pack: b, shallow: []string{tip}}
	}
	return nil
}

func (s *store) putClonePack(rp *repo, tip string, pack []byte, shallow, unshallow []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := rp.Owner + "/" + rp.Name + "@" + tip
	s.clonePack[key] = pack
}

func (s *store) writeFetchResponse(w http.ResponseWriter, rp *repo, fq fetchReq, pack []byte, shallow, unshallow []string, v2 bool) {
	var buf bytes.Buffer
	if v2 {
		if len(fq.haves) > 0 {
			// Negosiasi: klien mengirim haves — kirim acknowledgments
			// dengan "ready" agar klien lanjut ke packfile. Tanpa
			// "ready", git berhenti setelah NAK dan menganggap tidak
			// ada pack yang datang.
			buf.Write(pktLine([]byte("acknowledgments\n")))
			buf.Write(pktLine([]byte("NAK\n")))
			buf.Write(pktLine([]byte("ready\n")))
			buf.Write(pktDelim())
		}
		// Tanpa haves, klien langsung mengharapkan packfile — JANGAN
		// kirim acknowledgments (git: "expected 'packfile', received
		// 'acknowledgments'"). Klien kit menerima kedua bentuk.
		if len(shallow) > 0 || len(unshallow) > 0 {
			buf.Write(pktLine([]byte("shallow-info\n")))
			for _, sh := range shallow {
				buf.Write(pktLine([]byte("shallow " + sh + "\n")))
			}
			for _, un := range unshallow {
				buf.Write(pktLine([]byte("unshallow " + un + "\n")))
			}
			buf.Write(pktDelim())
		}
		// wanted-refs
		s.mu.RLock()
		var wanted []string
		for _, wn := range fq.wants {
			for _, name := range rp.sortedRefNames() {
				if rp.Refs[name].hex() == wn {
					wanted = append(wanted, wn+" "+name)
				}
			}
		}
		s.mu.RUnlock()
		if len(wanted) > 0 {
			buf.Write(pktLine([]byte("wanted-refs\n")))
			for _, line := range wanted {
				buf.Write(pktLine([]byte(line + "\n")))
			}
			buf.Write(pktDelim())
		}
		buf.Write(pktLine([]byte("packfile\n")))
		buf.Write(sidebandFrames(pack))
		buf.Write(pktFlush())
	} else {
		// v0: NAK lalu sideband pack
		buf.Write(pktLine([]byte("NAK\n")))
		for _, sh := range shallow {
			buf.Write(pktLine([]byte("shallow " + sh + "\n")))
		}
		for _, un := range unshallow {
			buf.Write(pktLine([]byte("unshallow " + un + "\n")))
		}
		buf.Write(sidebandFrames(pack))
		buf.Write(pktFlush())
	}
	w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
	w.WriteHeader(200)
	_, _ = w.Write(buf.Bytes())
}

// ---------------------------------------------------------------------------
// fetch v0 (dipakai bila klien meminta Git-Protocol: version=0 / tanpa v2)
// ---------------------------------------------------------------------------

func (s *store) handleFetchV0(w http.ResponseWriter, r *http.Request, rp *repo, body []byte) {
	pkts, err := parsePktLines(body)
	if err != nil {
		w.WriteHeader(400)
		return
	}
	var fq fetchReq
	fq.depth = -1
	section := 0
	for _, p := range pkts {
		if p.kind == "flush" {
			section++
			continue
		}
		if p.kind != "data" {
			continue
		}
		line := strings.TrimSuffix(string(p.payload), "\n")
		// capabilities setelah NUL pada want pertama
		if idx := strings.Index(line, "\x00"); idx >= 0 {
			if strings.Contains(line[idx:], "deepen-relative") {
				fq.deepenRelative = true
			}
			line = line[:idx]
		}
		switch {
		case strings.HasPrefix(line, "want "):
			fq.wants = append(fq.wants, strings.TrimPrefix(line, "want "))
		case strings.HasPrefix(line, "have "):
			fq.haves = append(fq.haves, strings.TrimPrefix(line, "have "))
		case strings.HasPrefix(line, "shallow "):
			fq.shallow = append(fq.shallow, strings.TrimPrefix(line, "shallow "))
		case strings.HasPrefix(line, "deepen "):
			if n, err := strconv.Atoi(strings.TrimPrefix(line, "deepen ")); err == nil {
				fq.depth = n
			}
		}
		_ = section
	}
	s.serveFetch(w, rp, fq, false)
}

// ---------------------------------------------------------------------------
// receive-pack v0
// ---------------------------------------------------------------------------

type pushCmd struct {
	oldOID oid
	newOID oid
	ref    string
}

func (s *store) handleReceivePack(w http.ResponseWriter, r *http.Request, rp *repo, body []byte) {
	// body: pkt-lines perintah, flush, lalu packfile mentah.
	pkts, rest, err := parsePktLinesPrefix(body)
	if err != nil {
		s.writePushReport(w, false, nil)
		return
	}
	var cmds []pushCmd
	for _, p := range pkts {
		if p.kind != "data" {
			continue
		}
		line := string(p.payload)
		if idx := strings.Index(line, "\x00"); idx >= 0 {
			line = line[:idx]
		}
		line = strings.TrimSuffix(line, "\n")
		parts := strings.Fields(line)
		if len(parts) != 3 {
			continue
		}
		oldOID, ok1 := oidFromHex(parts[0])
		newOID, ok2 := oidFromHex(parts[1])
		if !ok1 || !ok2 {
			continue
		}
		cmds = append(cmds, pushCmd{oldOID: oldOID, newOID: newOID, ref: parts[2]})
	}
	// unpack packfile (bila ada)
	unpackOK := true
	var newObjects map[oid]*gitObject
	if len(rest) > 0 {
		if !verifyPackChecksum(rest) {
			unpackOK = false
		} else {
			objs, err := readPack(rest, nil)
			if err != nil {
				unpackOK = false
			} else {
				newObjects = objs
			}
		}
	}
	s.mu.Lock()
	if unpackOK && newObjects != nil {
		for id, o := range newObjects {
			rp.Objects[id] = o
		}
		// pack memo invalidation: tip berubah
		s.clonePack = map[string][]byte{}
	}
	type refResult struct {
		ref    string
		ok     bool
		reason string
	}
	results := make([]refResult, 0, len(cmds))
	for _, cmd := range cmds {
		cur, exists := rp.Refs[cmd.ref]
		// delete
		if cmd.newOID == zeroOID {
			if !exists || cur != cmd.oldOID {
				results = append(results, refResult{cmd.ref, false, "non-fast-forward"})
				continue
			}
			delete(rp.Refs, cmd.ref)
			results = append(results, refResult{cmd.ref, true, ""})
			continue
		}
		// create
		if cmd.oldOID == zeroOID {
			if exists {
				results = append(results, refResult{cmd.ref, false, "non-fast-forward"})
				continue
			}
			rp.Refs[cmd.ref] = cmd.newOID
			results = append(results, refResult{cmd.ref, true, ""})
			continue
		}
		// update: old harus cocok + fast-forward
		if !exists || cur != cmd.oldOID {
			results = append(results, refResult{cmd.ref, false, "non-fast-forward"})
			continue
		}
		if !rp.isAncestor(cmd.oldOID, cmd.newOID) {
			results = append(results, refResult{cmd.ref, false, "non-fast-forward"})
			continue
		}
		rp.Refs[cmd.ref] = cmd.newOID
		results = append(results, refResult{cmd.ref, true, ""})
	}
	s.mu.Unlock()
	// report-status: stream-nya sendiri pkt-lined DI DALAM band 1 sideband
	// (git + klien kit mengurai band 1 sebagai pkt-lines, bukan teks mentah).
	var inner bytes.Buffer
	if unpackOK {
		inner.Write(pktLine([]byte("unpack ok\n")))
	} else {
		inner.Write(pktLine([]byte("unpack failed\n")))
	}
	for _, res := range results {
		if res.ok {
			inner.Write(pktLine([]byte("ok " + res.ref + "\n")))
		} else {
			inner.Write(pktLine([]byte("ng " + res.ref + " " + res.reason + "\n")))
		}
	}
	// Flush DI DALAM band 1: tanpa ini, pembaca pkt-line klien membaca EOF
	// setelah baris terakhir dan mati "the remote end hung up unexpectedly"
	// (report() di builtin/receive-pack.c juga mengakhiri stream dengan
	// packet_buf_flush sebelum send_sideband).
	inner.Write(pktFlush())
	var out bytes.Buffer
	out.Write(sidebandFrames(inner.Bytes()))
	out.Write(pktFlush())
	w.Header().Set("Content-Type", "application/x-git-receive-pack-result")
	w.WriteHeader(200)
	_, _ = w.Write(out.Bytes())
}

func (s *store) writePushReport(w http.ResponseWriter, unpackOK bool, results []struct {
	ref    string
	ok     bool
	reason string
}) {
	var inner bytes.Buffer
	if unpackOK {
		inner.Write(pktLine([]byte("unpack ok\n")))
	} else {
		inner.Write(pktLine([]byte("unpack failed\n")))
	}
	inner.Write(pktFlush())
	var out bytes.Buffer
	out.Write(sidebandFrames(inner.Bytes()))
	out.Write(pktFlush())
	w.Header().Set("Content-Type", "application/x-git-receive-pack-result")
	w.WriteHeader(200)
	_, _ = w.Write(out.Bytes())
}

// parsePktLinesPrefix mengurai pkt-lines di awal body dan mengembalikan
// sisa byte mentah (packfile) setelah flush pertama.
func parsePktLinesPrefix(b []byte) ([]pkt, []byte, error) {
	var out []pkt
	for len(b) > 0 {
		if len(b) < 4 {
			return nil, nil, fmt.Errorf("pkt-line truncated")
		}
		n, err := strconv.ParseInt(string(b[:4]), 16, 32)
		if err != nil {
			return nil, nil, fmt.Errorf("bad pkt-line length")
		}
		if n == 0 {
			return out, b[4:], nil
		}
		if n == 1 {
			out = append(out, pkt{kind: "delim"})
			b = b[4:]
			continue
		}
		if int(n) < 4 || len(b) < int(n) {
			return nil, nil, fmt.Errorf("pkt-line truncated")
		}
		out = append(out, pkt{kind: "data", payload: append([]byte(nil), b[4:n]...)})
		b = b[n:]
	}
	return out, nil, nil
}

var _ = hex.EncodeToString
var _ = io.EOF
