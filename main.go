package main

import (
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

func b64Encode(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

func b64Decode(s string) ([]byte, bool) {
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		// try raw (unpadded) encoding
		b, err = base64.RawStdEncoding.DecodeString(s)
		if err != nil {
			return nil, false
		}
	}
	return b, true
}

func parseTrialNow(v string) int64 {
	if v == "" {
		return DefaultEpoch
	}
	if n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64); err == nil {
		return n
	}
	if t, err := time.Parse(time.RFC3339, strings.TrimSpace(v)); err == nil {
		return t.Unix()
	}
	return DefaultEpoch
}

func main() {
	token := os.Getenv("MAKEFUN_SHOP_TOKEN")
	epoch := parseTrialNow(os.Getenv("MAKEFUN_TRIAL_NOW"))
	// The forge is one static Go binary: all state is in memory, no disk
	// I/O, no external git binary, no wall clock in any state value.
	store := NewStore(epoch, token)

	mux := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// request id on every REST reply (incl. 401/404)
		if strings.HasPrefix(r.URL.Path, "/api/v3") {
			w.Header().Set("X-GitHub-Request-Id", fmt.Sprintf("req-%d", store.nextReqID()))
		}
		switch {
		case r.URL.Path == "/healthz":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(200)
			w.Write([]byte(`{"ok":true}`))
		case r.URL.Path == "/api/graphql":
			store.handleGraphQL(w, r)
		case r.URL.Path == "/mcp":
			store.handleMCP(w, r)
		case strings.HasPrefix(r.URL.Path, "/api/v3"):
			store.handleREST(w, r)
		default:
			// git smart HTTP: /{owner}/{repo}.git/...
			owner, repoName, rest, ok := store.parseGitPath(r.URL.Path)
			if !ok {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(404)
				w.Write([]byte(`{"message":"Not Found"}`))
				return
			}
			if !store.gitAuth(r) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(401)
				w.Write([]byte(`{"message":"Requires authentication"}`))
				return
			}
			store.mu.RLock()
			repo := store.getRepo(owner, repoName)
			store.mu.RUnlock()
			if repo == nil {
				gitNotFound(w)
				return
			}
			body, _ := io.ReadAll(r.Body)
			switch {
			case rest == "info/refs":
				store.handleInfoRefs(w, r, repo, r.URL.Query().Get("service"))
			case rest == "git-upload-pack" && r.Method == "POST":
				store.handleUploadPack(w, r, repo, body)
			case rest == "git-receive-pack" && r.Method == "POST":
				store.handleReceivePack(w, r, repo, body)
			default:
				gitNotFound(w)
			}
		}
	})

	port := os.Getenv("FORGE_PORT")
	addr := "127.0.0.1:0"
	if port != "" {
		addr = "127.0.0.1:" + port
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "listen:", err)
		os.Exit(1)
	}
	if port == "" {
		fmt.Printf("PORT %d\n", ln.Addr().(*net.TCPAddr).Port)
	}
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
		fmt.Fprintln(os.Stderr, "serve:", err)
		os.Exit(1)
	}
}
