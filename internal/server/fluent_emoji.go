package server

import (
	"embed"
	"net/http"
	"strconv"
	"strings"
)

// The compact common set comes from microsoft/fluentui-emoji (MIT). Keeping
// it in the server package makes Web, Windows WebView2, and macOS WKWebView
// render the same offline assets without adding a JavaScript dependency.
//
//go:embed assets/fluent/*.png assets/fluent/LICENSE
var fluentEmojiAssets embed.FS

func serveFluentEmoji(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/__hfs/fluent-emoji/")
	if name == "" || strings.ContainsAny(name, `/\`) || !strings.HasSuffix(name, ".png") {
		http.NotFound(w, r)
		return
	}
	data, err := fluentEmojiAssets.ReadFile("assets/fluent/" + name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	if r.Method == http.MethodGet {
		_, _ = w.Write(data)
	}
}
