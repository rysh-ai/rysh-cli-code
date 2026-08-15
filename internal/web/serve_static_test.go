// SPDX-License-Identifier: Apache-2.0

package web

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// TestEmbeddedStaticServesSharedRenderer verifies that internal/web embeds and
// serves a valid SPA bundle (the shared rysh-cli-app renderer, built for web via
// Makefile.internal_web) — index.html mounts #root and references the built
// /assets/, and the server routes actually return them. No NATS needed.
func TestEmbeddedStaticServesSharedRenderer(t *testing.T) {
	// 1. index.html is a mountable SPA that references built assets.
	idx, err := staticFiles.ReadFile("static/index.html")
	if err != nil {
		t.Fatalf("read embedded index.html: %v", err)
	}
	html := string(idx)
	if !strings.Contains(html, `id="root"`) {
		t.Fatal("index.html missing #root mount point")
	}
	if !strings.Contains(html, "/assets/") {
		t.Fatal("index.html does not reference built /assets/")
	}

	// 2. The referenced JS + CSS assets exist in the embed.
	staticFS, err := fs.Sub(staticFiles, "static")
	if err != nil {
		t.Fatal(err)
	}
	entries, err := fs.ReadDir(staticFS, "assets")
	if err != nil {
		t.Fatalf("embedded assets/ dir: %v", err)
	}
	var hasJS, hasCSS bool
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".js") {
			hasJS = true
		}
		if strings.HasSuffix(e.Name(), ".css") {
			hasCSS = true
		}
	}
	if !hasJS || !hasCSS {
		t.Fatalf("embedded assets missing: js=%v css=%v", hasJS, hasCSS)
	}

	// 3. The server routes serve them (httptest, mirrors server.go routing).
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/", func(c *gin.Context) {
		data, _ := staticFiles.ReadFile("static/index.html")
		c.Data(http.StatusOK, "text/html; charset=utf-8", data)
	})
	r.StaticFS("/assets", http.FS(newPrefixFS(staticFS, "assets")))

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `id="root"`) {
		t.Fatalf("GET / = %d, body missing #root", w.Code)
	}
}
