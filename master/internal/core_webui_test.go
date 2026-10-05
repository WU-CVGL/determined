package internal

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/require"
)

// TestWebUICaching checks the Cache-Control of the web UI's files. Browsers must revalidate the
// HTML pages, which name the bundle's hashed files: a page reused from an earlier release would
// run that release's web UI against the master.
func TestWebUICaching(t *testing.T) {
	reactRoot := t.TempDir()
	files := map[string]string{
		"index.html":              "react index",
		"design/index.html":       "design index",
		"assets/index-abc123.js":  "hashed script",
		"assets/index-abc123.css": "hashed style",
		"favicon.ico":             "icon",
		"robots.txt":              "robots",
	}
	for name, content := range files {
		path := filepath.Join(reactRoot, name)
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o750))
		require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	}
	e := echo.New()
	require.NoError(t, registerWebUIRoutes(e.Group(webuiBaseRoute), reactRoot))

	get := func(path string, header http.Header) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		for k, v := range header {
			req.Header[k] = v
		}
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)
		return rec
	}

	for path, want := range map[string]string{
		"/det":                   "react index",
		"/det/":                  "react index",
		"/det/index.html":        "react index",
		"/det/some/spa/route":    "react index",
		"/det/experiments/7":     "react index",
		"/det/assets/":           "react index",
		"/det/design":            "design index",
		"/det/design/":           "design index",
		"/det/design/index.html": "design index",
		// A hashed file of an earlier release, asked for by a page loaded before the upgrade.
		"/det/assets/index-0ld999.js": "react index",
	} {
		resp := get(path, nil)
		require.Equal(t, http.StatusOK, resp.Code, path)
		require.Equal(t, "no-cache", resp.Header().Get("Cache-Control"), path)
		require.Equal(t, want, resp.Body.String(), path)
	}

	for path, cacheControl := range map[string]string{
		"/det/assets/index-abc123.js":  "public, max-age=31536000",
		"/det/assets/index-abc123.css": "public, max-age=31536000",
		"/det/favicon.ico":             "public, max-age=600",
		"/det/robots.txt":              "",
	} {
		resp := get(path, nil)
		require.Equal(t, http.StatusOK, resp.Code, path)
		require.Equal(t, cacheControl, resp.Header().Get("Cache-Control"), path)
		require.Equal(t, files[strings.TrimPrefix(path, webuiBaseRoute+"/")], resp.Body.String(), path)
	}

	// Revalidating an unchanged index costs a 304, which keeps the header.
	for _, path := range []string{"/det/", "/det/some/spa/route", "/det/design/"} {
		lastModified := get(path, nil).Header().Get("Last-Modified")
		require.NotEmpty(t, lastModified, path)
		resp := get(path, http.Header{"If-Modified-Since": {lastModified}})
		require.Equal(t, http.StatusNotModified, resp.Code, path)
		require.Equal(t, "no-cache", resp.Header().Get("Cache-Control"), path)
	}
}
