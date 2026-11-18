// Forge Go: satu static binary, in-memory, tanpa disk I/O, tanpa git
// eksternal, tanpa internet. Melayani git smart HTTP, REST v3, GraphQL,
// dan MCP dari satu store. Port dari env FORGE_PORT (dipilih main.ts);
// MAKEFUN_SHOP_TOKEN adalah bearer token yang harus dibawa setiap request.
package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
)

var st *store

func main() {
	initBaseNow()
	token := os.Getenv("MAKEFUN_SHOP_TOKEN")
	st = newStore(token)
	port := os.Getenv("FORGE_PORT")
	if port == "" {
		port = "0"
	}
	ln, err := net.Listen("tcp", "0.0.0.0:"+port)
	if err != nil {
		os.Stderr.WriteString("[forge] listen: " + err.Error() + "\n")
		os.Exit(1)
	}
	actual := ln.Addr().(*net.TCPAddr).Port
	os.Stdout.WriteString("[forge] listening on port " + strconv.Itoa(actual) + "\n")
	srv := &http.Server{
		Handler:      http.HandlerFunc(serveHTTP),
		ReadTimeout:  0,
		WriteTimeout: 0,
	}
	if err := srv.Serve(ln); err != nil {
		os.Stderr.WriteString("[forge] serve: " + err.Error() + "\n")
		os.Exit(1)
	}
}

// ---------------------------------------------------------------------------
// Helpers HTTP
// ---------------------------------------------------------------------------

func reqID() string {
	n := atomic.AddUint64(&st.reqSeq, 1)
	return "forge-" + strconv.FormatUint(n, 10)
}

// jsonReply menulis JSON dengan header standar REST.
func jsonReply(w http.ResponseWriter, status int, body []byte) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-GitHub-Request-Id", reqID())
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func jsonErr(w http.ResponseWriter, status int, msg string) {
	b, _ := json.Marshal(map[string]string{"message": msg})
	jsonReply(w, status, b)
}

// readBody membaca body request (batas 32 MiB).
func readBody(r *http.Request) ([]byte, bool) {
	b, err := io.ReadAll(io.LimitReader(r.Body, 32<<20))
	if err != nil {
		return nil, false
	}
	return b, true
}

// decodeJSON mengurai body JSON; mengembalikan nil bila body kosong.
func decodeJSON(b []byte) (map[string]any, bool) {
	if len(bytesTrimSpace(b)) == 0 {
		return map[string]any{}, true
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, false
	}
	return m, true
}

func bytesTrimSpace(b []byte) []byte {
	return []byte(strings.TrimSpace(string(b)))
}

func strField(m map[string]any, key string) string {
	if v, ok := m[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

func boolField(m map[string]any, key string, def bool) bool {
	if v, ok := m[key]; ok {
		if b, ok := v.(bool); ok {
			return b
		}
	}
	return def
}

func intField(m map[string]any, key string, def int) int {
	if v, ok := m[key]; ok {
		switch n := v.(type) {
		case float64:
			return int(n)
		case int:
			return n
		}
	}
	return def
}

// ---------------------------------------------------------------------------
// Routing
// ---------------------------------------------------------------------------

func serveHTTP(w http.ResponseWriter, r *http.Request) {
	// /healthz tanpa auth (boot check env).
	if r.Method == "GET" && r.URL.Path == "/healthz" {
		jsonReply(w, 200, []byte(`{"ok":true}`))
		return
	}
	// Auth: Bearer MAKEFUN_SHOP_TOKEN wajib di semua permukaan lain.
	if st.token != "" {
		auth := r.Header.Get("Authorization")
		if auth != "Bearer "+st.token {
			// Git + MCP + GraphQL + REST semuanya 401 JSON (kecuali git
			// info/refs yang klien baca sebagai GitHttpError — tetap 401).
			jsonErr(w, 401, "Requires authentication")
			return
		}
	}
	path := r.URL.Path
	switch {
	case path == "/api/graphql" && r.Method == "POST":
		handleGraphQL(w, r)
	case path == "/mcp" && r.Method == "POST":
		handleMCP(w, r)
	case path == "/mcp" && r.Method == "DELETE":
		// MCP session teardown -> 204 kosong.
		w.Header().Set("X-GitHub-Request-Id", reqID())
		w.WriteHeader(204)
	case strings.HasPrefix(path, "/api/v3"):
		handleREST(w, r)
	case strings.HasSuffix(path, ".git/info/refs"):
		handleGitInfoRefs(w, r)
	case strings.HasSuffix(path, ".git/git-upload-pack"):
		handleGitUploadPack(w, r)
	case strings.HasSuffix(path, ".git/git-receive-pack"):
		handleGitReceivePack(w, r)
	default:
		jsonErr(w, 404, "Not Found")
	}
}

// gitRepoFromPath mengurai /{owner}/{repo}.git/... -> owner, repo, repo object.
// Mengembalikan nil bila repo tidak ada (caller menjawab 404).
func gitRepoFromPath(path string) (string, string, *repo) {
	// path: /owner/repo.git/...
	trim := strings.TrimPrefix(path, "/")
	parts := strings.SplitN(trim, "/", 3)
	if len(parts) < 2 {
		return "", "", nil
	}
	owner := parts[0]
	repoName := strings.TrimSuffix(parts[1], ".git")
	if owner == "" || repoName == "" {
		return "", "", nil
	}
	st.mu.RLock()
	defer st.mu.RUnlock()
	rp := st.getRepo(owner, repoName)
	return owner, repoName, rp
}

func handleGitInfoRefs(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		jsonErr(w, 405, "Method Not Allowed")
		return
	}
	_, _, rp := gitRepoFromPath(r.URL.Path)
	if rp == nil {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		jsonErr(w, 404, "Not Found")
		return
	}
	service := r.URL.Query().Get("service")
	if service == "git-receive-pack" {
		st.infoRefsReceive(w, r, rp)
		return
	}
	st.infoRefsUpload(w, r, rp)
}

func handleGitUploadPack(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		jsonErr(w, 405, "Method Not Allowed")
		return
	}
	_, _, rp := gitRepoFromPath(r.URL.Path)
	if rp == nil {
		jsonErr(w, 404, "Not Found")
		return
	}
	body, ok := readBody(r)
	if !ok {
		w.WriteHeader(400)
		return
	}
	if r.Header.Get("Git-Protocol") == "version=2" {
		// Bedakan ls-refs vs fetch dari isi pkt-line pertama.
		if bytes.Contains(body, []byte("command=ls-refs")) {
			st.handleLsRefs(w, r, rp, body)
			return
		}
		st.handleFetchV2(w, r, rp, body)
		return
	}
	// Tanpa v2: klien v0 mengirim want/have — perlakukan sebagai fetch v0.
	// Bila body bukan pkt-line valid (malformed probe), jangan crash.
	if len(body) >= 4 && (body[0] < '0' || body[0] > '9') {
		w.WriteHeader(400)
		return
	}
	st.handleFetchV0(w, r, rp, body)
}

func handleGitReceivePack(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		jsonErr(w, 405, "Method Not Allowed")
		return
	}
	_, _, rp := gitRepoFromPath(r.URL.Path)
	if rp == nil {
		jsonErr(w, 404, "Not Found")
		return
	}
	body, ok := readBody(r)
	if !ok {
		w.WriteHeader(400)
		return
	}
	st.handleReceivePack(w, r, rp, body)
}
