package internal

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	"github.com/stretchr/testify/require"
)

func gzipped(t *testing.T, b []byte) []byte {
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	_, err := w.Write(b)
	require.NoError(t, err)
	require.NoError(t, w.Close())
	return buf.Bytes()
}

// TestGzipSkipper checks that the master compresses the API's and the static files' responses
// once, and leaves a proxied response as the task sent it: an icon the task serves gzipped reaches
// the client with one layer of gzip.
func TestGzipSkipper(t *testing.T) {
	svg := []byte(`<svg xmlns="http://www.w3.org/2000/svg"></svg>`)
	e := echo.New()
	e.Use(middleware.GzipWithConfig(middleware.GzipConfig{Skipper: gzipSkipper}))
	e.GET("/proxy/:service/*", func(c echo.Context) error {
		c.Response().Header().Set(echo.HeaderContentEncoding, "gzip")
		return c.Blob(http.StatusOK, "image/svg+xml", gzipped(t, svg))
	})
	e.GET("/det/*", func(c echo.Context) error {
		return c.Blob(http.StatusOK, "image/svg+xml", svg)
	})
	e.GET("/api/v1/master", func(c echo.Context) error {
		return c.JSON(http.StatusOK, map[string]string{"version": "x"})
	})
	get := func(path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set(echo.HeaderAcceptEncoding, "gzip")
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)
		require.Equal(t, http.StatusOK, rec.Code, path)
		return rec
	}
	gunzip := func(b []byte) []byte {
		r, err := gzip.NewReader(bytes.NewReader(b))
		require.NoError(t, err)
		out, err := io.ReadAll(r)
		require.NoError(t, err)
		return out
	}

	proxied := get("/proxy/task:2984/icon_bundle.svg")
	require.Equal(t, []string{"gzip"}, proxied.Header().Values(echo.HeaderContentEncoding))
	require.Equal(t, svg, gunzip(proxied.Body.Bytes()))

	static := get("/det/assets/logo.svg")
	require.Equal(t, []string{"gzip"}, static.Header().Values(echo.HeaderContentEncoding))
	require.Equal(t, svg, gunzip(static.Body.Bytes()))

	api := get("/api/v1/master")
	require.Equal(t, []string{"gzip"}, api.Header().Values(echo.HeaderContentEncoding))
	require.JSONEq(t, `{"version": "x"}`, string(gunzip(api.Body.Bytes())))
}
