package isuports

// TLS を各アプリノードで直接終端するためのフロント。
// isu1 の nginx は 443 を L4 (stream, SNI ハッシュ) で流すだけにして、TLS・HTTP/2 の CPU を 3 台に分散する。
// nginx(http) がやっていた仕事（静的ファイル、/auth/ のプロキシ）もここで受ける。

import (
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

func frontHandler(api http.Handler) http.Handler {
	publicDir := getEnv("ISUCON_PUBLIC_DIR", "../../public")
	fs := http.FileServer(http.Dir(publicDir))
	authURL, _ := url.Parse("http://" + getEnv("ISUCON_AUTH_ADDR", "127.0.0.1:3001"))
	auth := httputil.NewSingleHostReverseProxy(authURL)
	serveIndex := func(w http.ResponseWriter, r *http.Request) {
		f, err := os.Open(filepath.Join(publicDir, "index.html"))
		if err != nil {
			http.NotFound(w, r)
			return
		}
		defer f.Close()
		http.ServeContent(w, r, "index.html", time.Time{}, f)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		switch {
		case strings.HasPrefix(p, "/api") || strings.HasPrefix(p, "/initialize"):
			api.ServeHTTP(w, r)
		case strings.HasPrefix(p, "/auth/"):
			auth.ServeHTTP(w, r)
		default:
			// nginx の try_files $uri /index.html 相当
			clean := path.Clean("/" + p)
			if clean == "/index.html" {
				serveIndex(w, r)
				return
			}
			if fi, err := os.Stat(filepath.Join(publicDir, filepath.FromSlash(clean))); err != nil || fi.IsDir() {
				serveIndex(w, r)
				return
			}
			fs.ServeHTTP(w, r)
		}
	})
}
