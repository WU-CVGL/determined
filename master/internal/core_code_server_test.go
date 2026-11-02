package internal

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/require"

	"github.com/determined-ai/determined/master/internal/config"
)

const (
	testCodeServerVersion = "4.144.0"
	testCodeServerAMD64   = "code-server-4.144.0-linux-amd64.tar.gz"
	testCodeServerARM64   = "code-server-4.144.0-linux-arm64.tar.gz"
)

// testCodeServerContent is a stand-in for the tarball of arch.
func testCodeServerContent(arch string) []byte {
	return bytes.Repeat([]byte("code-server "+arch+" "), 20000)
}

func testCodeServerSum(arch string) string {
	sum := sha256.Sum256(testCodeServerContent(arch))
	return hex.EncodeToString(sum[:])
}

type codeServerTest struct {
	artifacts *codeServerArtifacts
	echo      *echo.Echo
	dir       string
	// calls counts the requests that reached the upstream server.
	calls atomic.Int32
}

// newCodeServerTest serves the code-server route of a master whose upstream is handler, or, with
// a nil handler, the release files of both architectures.
func newCodeServerTest(
	t *testing.T, handler http.HandlerFunc, modify func(*config.CodeServerConfig),
) *codeServerTest {
	cst := &codeServerTest{dir: t.TempDir()}
	if handler == nil {
		handler = func(w http.ResponseWriter, r *http.Request) {
			for _, arch := range config.CodeServerArches {
				if r.URL.Path == "/v4.144.0/code-server-4.144.0-linux-"+arch+".tar.gz" {
					_, _ = w.Write(testCodeServerContent(arch))
					return
				}
			}
			http.NotFound(w, r)
		}
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cst.calls.Add(1)
		handler(w, r)
	}))
	t.Cleanup(upstream.Close)

	conf := config.CodeServerConfig{
		Version:     testCodeServerVersion,
		Dir:         filepath.Join(cst.dir, "code-server"),
		URLTemplate: upstream.URL + "/v{version}/code-server-{version}-linux-{arch}.tar.gz",
	}
	if modify != nil {
		modify(&conf)
	}
	artifacts, err := newCodeServerArtifacts(conf)
	require.NoError(t, err)
	cst.artifacts = artifacts
	cst.dir = conf.Dir
	cst.echo = echo.New()
	cst.echo.GET(codeServerRoute, artifacts.serve)
	return cst
}

func (cst *codeServerTest) get(path string, header ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	rec := httptest.NewRecorder()
	cst.echo.ServeHTTP(rec, req)
	return rec
}

// files lists the names in the cache directory.
func (cst *codeServerTest) files(t *testing.T) []string {
	entries, err := os.ReadDir(cst.dir)
	require.NoError(t, err)
	names := []string{}
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names
}

func TestCodeServerNotFound(t *testing.T) {
	cst := newCodeServerTest(t, nil, nil)
	for _, path := range []string{
		"/det/code-server/",
		"/det/code-server/4.144.0",
		"/det/code-server/4.144.0/",
		"/det/code-server/4.143.0/code-server-4.143.0-linux-amd64.tar.gz",
		"/det/code-server/4.144.0/code-server-4.143.0-linux-amd64.tar.gz",
		"/det/code-server/4.143.0/code-server-4.144.0-linux-amd64.tar.gz",
		"/det/code-server/4.144.0/code-server-4.144.0-linux-armhf.tar.gz",
		"/det/code-server/4.144.0/code-server-4.144.0-linux-x86_64.tar.gz",
		"/det/code-server/4.144.0/code-server-4.144.0-darwin-amd64.tar.gz",
		"/det/code-server/4.144.0/code-server-4.144.0-linux-amd64.tar",
		"/det/code-server/4.144.0/code-server-4.144.0-linux-amd64.tar.gz.tmp",
		"/det/code-server/4.144.0/code-server-4.144.0-linux-amd64.tar.gz.sha256.tmp",
		"/det/code-server/4.144.0/code-server-4.144.0-linux-amd64.tar.gz/",
		"/det/code-server/4.144.0/code-server_4.144.0_amd64.deb",
		"/det/code-server/4.144.0/x/code-server-4.144.0-linux-amd64.tar.gz",
		"/det/code-server/4.144.0/../4.144.0/code-server-4.144.0-linux-amd64.tar.gz",
		"/det/code-server/4.144.0/..%2F4.144.0%2Fcode-server-4.144.0-linux-amd64.tar.gz",
		"/det/code-server/4.144.0%2Fcode-server-4.144.0-linux-amd64.tar.gz",
		"/det/code-server/4.144.0/code-server-4.144.0-linux-amd64%2Etar%2Egz",
		"/det/code-server/%2E%2E/code-server-4.144.0-linux-amd64.tar.gz",
		"/det/code-server/4.144.0/..",
	} {
		require.Equal(t, http.StatusNotFound, cst.get(path).Code, path)
	}
	require.Equal(t, int32(0), cst.calls.Load())
	require.Empty(t, cst.files(t))

	// Without the section, nothing is served.
	disabled := echo.New()
	disabled.GET(codeServerRoute, (*codeServerArtifacts)(nil).serve)
	rec := httptest.NewRecorder()
	disabled.ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		"/det/code-server/4.144.0/"+testCodeServerAMD64, nil))
	require.Equal(t, http.StatusNotFound, rec.Code)
}

func TestCodeServerLazyDownload(t *testing.T) {
	cst := newCodeServerTest(t, nil, nil)
	require.Empty(t, cst.files(t))

	rec := cst.get("/det/code-server/4.144.0/" + testCodeServerARM64)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, testCodeServerContent("arm64"), rec.Body.Bytes())
	require.Equal(t, "application/gzip", rec.Header().Get("Content-Type"))
	require.Equal(t, int32(1), cst.calls.Load())
	require.Equal(t, []string{testCodeServerARM64, testCodeServerARM64 + ".sha256"}, cst.files(t))
	sumLine, err := os.ReadFile(filepath.Join(cst.dir, testCodeServerARM64+".sha256"))
	require.NoError(t, err)
	require.Equal(t, testCodeServerSum("arm64")+"  "+testCodeServerARM64+"\n", string(sumLine))

	// Later requests, for the tarball or its checksum, are served from the directory.
	rec = cst.get("/det/code-server/4.144.0/" + testCodeServerARM64)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, testCodeServerContent("arm64"), rec.Body.Bytes())
	rec = cst.get("/det/code-server/4.144.0/" + testCodeServerARM64 + ".sha256")
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, string(sumLine), rec.Body.String())
	require.Equal(t, int32(1), cst.calls.Load())

	// The first request for the checksum of the other architecture downloads its tarball.
	rec = cst.get("/det/code-server/4.144.0/" + testCodeServerAMD64 + ".sha256")
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, testCodeServerSum("amd64")+"  "+testCodeServerAMD64+"\n", rec.Body.String())
	require.Equal(t, int32(2), cst.calls.Load())
	require.Equal(t, []string{
		testCodeServerAMD64, testCodeServerAMD64 + ".sha256",
		testCodeServerARM64, testCodeServerARM64 + ".sha256",
	}, cst.files(t))
}

func TestCodeServerConfiguredChecksum(t *testing.T) {
	cst := newCodeServerTest(t, nil, func(c *config.CodeServerConfig) {
		c.SHA256 = map[string]string{"amd64": strings.ToUpper(testCodeServerSum("amd64"))}
	})
	rec := cst.get("/det/code-server/4.144.0/" + testCodeServerAMD64)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, testCodeServerContent("amd64"), rec.Body.Bytes())

	cst = newCodeServerTest(t, nil, func(c *config.CodeServerConfig) {
		c.SHA256 = map[string]string{"amd64": testCodeServerSum("arm64")}
	})
	rec = cst.get("/det/code-server/4.144.0/" + testCodeServerAMD64)
	require.Equal(t, http.StatusBadGateway, rec.Code)
	require.Contains(t, rec.Body.String(), "code-server checksum mismatch")
	require.NotContains(t, rec.Body.String(), testCodeServerSum("amd64"))
	require.Empty(t, cst.files(t))
	rec = cst.get("/det/code-server/4.144.0/" + testCodeServerAMD64 + ".sha256")
	require.Equal(t, http.StatusBadGateway, rec.Code)
	require.Empty(t, cst.files(t))
	require.Equal(t, int32(2), cst.calls.Load(), "a failed download is tried again")
}

func TestCodeServerUpstreamErrors(t *testing.T) {
	t.Run("status", func(t *testing.T) {
		cst := newCodeServerTest(t, func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "unavailable", http.StatusInternalServerError)
		}, nil)
		rec := cst.get("/det/code-server/4.144.0/" + testCodeServerAMD64)
		require.Equal(t, http.StatusBadGateway, rec.Code)
		require.Contains(t, rec.Body.String(), "code-server download failed")
		require.NotContains(t, rec.Body.String(), "127.0.0.1")
		require.Empty(t, cst.files(t))
	})

	t.Run("timeout", func(t *testing.T) {
		cst := newCodeServerTest(t, func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write(testCodeServerContent("amd64")[:1000])
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		}, nil)
		cst.artifacts.client.Timeout = 200 * time.Millisecond
		rec := cst.get("/det/code-server/4.144.0/" + testCodeServerAMD64)
		require.Equal(t, http.StatusBadGateway, rec.Code)
		require.Empty(t, cst.files(t))
	})

	t.Run("network", func(t *testing.T) {
		cst := newCodeServerTest(t, nil, func(c *config.CodeServerConfig) {
			closed := httptest.NewServer(http.NotFoundHandler())
			closed.Close()
			c.URLTemplate = closed.URL + "/{version}/{arch}"
		})
		rec := cst.get("/det/code-server/4.144.0/" + testCodeServerAMD64)
		require.Equal(t, http.StatusBadGateway, rec.Code)
		require.Empty(t, cst.files(t))
	})
}

func TestCodeServerConcurrentFirstRequests(t *testing.T) {
	release := make(chan struct{})
	cst := newCodeServerTest(t, func(w http.ResponseWriter, r *http.Request) {
		<-release
		_, _ = w.Write(testCodeServerContent("amd64"))
	}, nil)

	const n = 8
	var wg sync.WaitGroup
	recs := make([]*httptest.ResponseRecorder, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			path := "/det/code-server/4.144.0/" + testCodeServerAMD64
			if i%2 == 1 {
				path += ".sha256"
			}
			recs[i] = cst.get(path)
		}(i)
	}
	require.Eventually(t, func() bool { return cst.calls.Load() == 1 }, 10*time.Second, 10*time.Millisecond)
	time.Sleep(100 * time.Millisecond)
	close(release)
	wg.Wait()

	require.Equal(t, int32(1), cst.calls.Load())
	for i, rec := range recs {
		require.Equal(t, http.StatusOK, rec.Code, i)
		if i%2 == 0 {
			require.Equal(t, testCodeServerContent("amd64"), rec.Body.Bytes(), i)
		} else {
			require.Equal(t, testCodeServerSum("amd64")+"  "+testCodeServerAMD64+"\n", rec.Body.String(), i)
		}
	}
	require.Equal(t, []string{testCodeServerAMD64, testCodeServerAMD64 + ".sha256"}, cst.files(t))
}

func TestCodeServerPlacedFile(t *testing.T) {
	place := func(cst *codeServerTest) {
		require.NoError(t, os.WriteFile(filepath.Join(cst.dir, testCodeServerAMD64),
			testCodeServerContent("amd64"), 0o600))
	}

	cst := newCodeServerTest(t, nil, nil)
	place(cst)
	rec := cst.get("/det/code-server/4.144.0/" + testCodeServerAMD64 + ".sha256")
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, testCodeServerSum("amd64")+"  "+testCodeServerAMD64+"\n", rec.Body.String())
	rec = cst.get("/det/code-server/4.144.0/" + testCodeServerAMD64)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, testCodeServerContent("amd64"), rec.Body.Bytes())
	require.Equal(t, []string{testCodeServerAMD64, testCodeServerAMD64 + ".sha256"}, cst.files(t))
	require.Equal(t, int32(0), cst.calls.Load())

	// A placed file that does not match the configured checksum is refused and kept.
	cst = newCodeServerTest(t, nil, func(c *config.CodeServerConfig) {
		c.SHA256 = map[string]string{"amd64": testCodeServerSum("arm64")}
	})
	place(cst)
	rec = cst.get("/det/code-server/4.144.0/" + testCodeServerAMD64)
	require.Equal(t, http.StatusBadGateway, rec.Code)
	require.Equal(t, []string{testCodeServerAMD64}, cst.files(t))
	require.Equal(t, int32(0), cst.calls.Load())
}

func TestCodeServerLeftoverTmpOverwritten(t *testing.T) {
	cst := newCodeServerTest(t, nil, nil)
	require.NoError(t, os.WriteFile(filepath.Join(cst.dir, testCodeServerAMD64+".tmp"),
		bytes.Repeat([]byte("x"), 5_000_000), 0o600))
	rec := cst.get("/det/code-server/4.144.0/" + testCodeServerAMD64)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, testCodeServerContent("amd64"), rec.Body.Bytes())
	require.Equal(t, []string{testCodeServerAMD64, testCodeServerAMD64 + ".sha256"}, cst.files(t))
}

func TestCodeServerRange(t *testing.T) {
	cst := newCodeServerTest(t, nil, nil)
	rec := cst.get("/det/code-server/4.144.0/"+testCodeServerAMD64, "Range", "bytes=10-19")
	require.Equal(t, http.StatusPartialContent, rec.Code)
	content := testCodeServerContent("amd64")
	require.Equal(t, content[10:20], rec.Body.Bytes())
	require.Equal(t, "bytes 10-19/"+strconv.Itoa(len(content)), rec.Header().Get("Content-Range"))

	rec = cst.get("/det/code-server/4.144.0/" + testCodeServerAMD64)
	require.Equal(t, strconv.Itoa(len(content)), rec.Header().Get("Content-Length"))
}

func TestRedactURL(t *testing.T) {
	for _, tc := range [][2]string{
		{"https://mirror.example/releases/4.144.0/amd64.tar.gz", "https://mirror.example/releases/4.144.0/amd64.tar.gz"},
		{"https://user:secret@mirror.example:8443/a/b.tar.gz?token=secret#f", "https://mirror.example:8443/a/b.tar.gz"},
		{"http://mirror.example/a%2Fb.tar.gz?X-Signature=secret", "http://mirror.example/a%2Fb.tar.gz"},
		{"https://mirror.example?token=secret", "https://mirror.example"},
		{"https://user:secret@mirror.example/%zz?token=secret", "(invalid URL)"},
	} {
		require.Equal(t, tc[1], redactURL(tc[0]), tc[0])
	}
}

func TestCodeServerUpstreamErrorsRedactURL(t *testing.T) {
	const secret = "query-secret"
	t.Run("status", func(t *testing.T) {
		cst := newCodeServerTest(t, func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "unavailable", http.StatusInternalServerError)
		}, func(c *config.CodeServerConfig) { c.URLTemplate += "?token=" + secret })
		err := cst.artifacts.prepare(testCodeServerVersion, "amd64")
		require.ErrorIs(t, err, errCodeServerUpstream)
		require.Contains(t, err.Error(), "/code-server-4.144.0-linux-amd64.tar.gz answered 500")
		require.NotContains(t, err.Error(), secret)
	})

	t.Run("network", func(t *testing.T) {
		cst := newCodeServerTest(t, nil, func(c *config.CodeServerConfig) {
			closed := httptest.NewServer(http.NotFoundHandler())
			closed.Close()
			c.URLTemplate = closed.URL + "/{version}/{arch}?token=" + secret
		})
		err := cst.artifacts.prepare(testCodeServerVersion, "amd64")
		require.ErrorIs(t, err, errCodeServerUpstream)
		require.Contains(t, err.Error(), "/4.144.0/amd64: ")
		require.NotContains(t, err.Error(), secret)
	})

	t.Run("redirect", func(t *testing.T) {
		cst := newCodeServerTest(t, func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, r.URL.String(), http.StatusFound)
		}, func(c *config.CodeServerConfig) { c.URLTemplate += "?token=" + secret })
		err := cst.artifacts.prepare(testCodeServerVersion, "amd64")
		require.ErrorIs(t, err, errCodeServerUpstream)
		require.Contains(t, err.Error(), "redirects")
		require.NotContains(t, err.Error(), secret)
	})
}

// placeCodeServerFile writes content to the file at path in place, as an administrator would, with
// a modification time that differs for each n so that a rewrite of the same size is a change.
func placeCodeServerFile(t *testing.T, path string, content []byte, n int) {
	require.NoError(t, os.WriteFile(path, content, 0o600))
	mtime := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(n) * time.Hour)
	require.NoError(t, os.Chtimes(path, mtime, mtime))
}

func TestCodeServerReplacedTarball(t *testing.T) {
	amd64Line := testCodeServerSum("amd64") + "  " + testCodeServerAMD64 + "\n"
	arm64Line := testCodeServerSum("arm64") + "  " + testCodeServerAMD64 + "\n"

	// Without a configured checksum, a replaced tarball is hashed again and its checksum file
	// rewritten, although that still matches the file name.
	cst := newCodeServerTest(t, nil, nil)
	path := filepath.Join(cst.dir, testCodeServerAMD64)
	placeCodeServerFile(t, path, testCodeServerContent("amd64"), 1)
	require.Equal(t, amd64Line, cst.get("/det/code-server/4.144.0/"+testCodeServerAMD64+".sha256").Body.String())
	placeCodeServerFile(t, path, testCodeServerContent("arm64"), 2)
	rec := cst.get("/det/code-server/4.144.0/" + testCodeServerAMD64 + ".sha256")
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, arm64Line, rec.Body.String())
	sumLine, err := os.ReadFile(filepath.Join(cst.dir, testCodeServerAMD64+".sha256"))
	require.NoError(t, err)
	require.Equal(t, arm64Line, string(sumLine))
	require.Equal(t, int32(2), cst.artifacts.hashed.Load())

	// With one, the replaced tarball is refused and kept.
	cst = newCodeServerTest(t, nil, func(c *config.CodeServerConfig) {
		c.SHA256 = map[string]string{"amd64": testCodeServerSum("amd64")}
	})
	path = filepath.Join(cst.dir, testCodeServerAMD64)
	placeCodeServerFile(t, path, testCodeServerContent("amd64"), 1)
	require.Equal(t, http.StatusOK, cst.get("/det/code-server/4.144.0/"+testCodeServerAMD64).Code)
	placeCodeServerFile(t, path, testCodeServerContent("arm64"), 2)
	require.Equal(t, http.StatusBadGateway, cst.get("/det/code-server/4.144.0/"+testCodeServerAMD64).Code)
	require.Equal(t, http.StatusBadGateway,
		cst.get("/det/code-server/4.144.0/"+testCodeServerAMD64+".sha256").Code)
	content, err := os.ReadFile(filepath.Join(cst.dir, testCodeServerAMD64))
	require.NoError(t, err)
	require.Equal(t, testCodeServerContent("arm64"), content)
	require.Equal(t, int32(0), cst.calls.Load())
}

func TestCodeServerRepairedTarball(t *testing.T) {
	cst := newCodeServerTest(t, nil, func(c *config.CodeServerConfig) {
		c.SHA256 = map[string]string{"amd64": testCodeServerSum("amd64")}
	})
	path := filepath.Join(cst.dir, testCodeServerAMD64)
	placeCodeServerFile(t, path, testCodeServerContent("arm64"), 1)
	require.NoError(t, os.WriteFile(path+".sha256",
		[]byte(testCodeServerSum("arm64")+"  "+testCodeServerAMD64+"\n"), 0o600))
	require.Equal(t, http.StatusBadGateway, cst.get("/det/code-server/4.144.0/"+testCodeServerAMD64).Code)

	// The repaired tarball is served although its checksum file is stale, which is rewritten.
	placeCodeServerFile(t, path, testCodeServerContent("amd64"), 2)
	rec := cst.get("/det/code-server/4.144.0/" + testCodeServerAMD64)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, testCodeServerContent("amd64"), rec.Body.Bytes())
	sumLine, err := os.ReadFile(filepath.Join(cst.dir, testCodeServerAMD64+".sha256"))
	require.NoError(t, err)
	require.Equal(t, testCodeServerSum("amd64")+"  "+testCodeServerAMD64+"\n", string(sumLine))
	require.Equal(t, int32(0), cst.calls.Load())
}

func TestCodeServerHashesOnce(t *testing.T) {
	cst := newCodeServerTest(t, nil, nil)
	path := filepath.Join(cst.dir, testCodeServerAMD64)
	placeCodeServerFile(t, path, testCodeServerContent("amd64"), 1)
	for _, p := range []string{testCodeServerAMD64, testCodeServerAMD64 + ".sha256", testCodeServerAMD64} {
		require.Equal(t, http.StatusOK, cst.get("/det/code-server/4.144.0/"+p).Code, p)
	}
	require.Equal(t, int32(1), cst.artifacts.hashed.Load())

	// A deleted checksum file is written again without hashing.
	require.NoError(t, os.Remove(path+".sha256"))
	rec := cst.get("/det/code-server/4.144.0/" + testCodeServerAMD64 + ".sha256")
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, testCodeServerSum("amd64")+"  "+testCodeServerAMD64+"\n", rec.Body.String())
	require.Equal(t, int32(1), cst.artifacts.hashed.Load())

	// A downloaded tarball is hashed while it is downloaded only.
	for _, p := range []string{testCodeServerARM64, testCodeServerARM64 + ".sha256", testCodeServerARM64} {
		require.Equal(t, http.StatusOK, cst.get("/det/code-server/4.144.0/"+p).Code, p)
	}
	require.Equal(t, int32(1), cst.artifacts.hashed.Load())
	require.Equal(t, int32(1), cst.calls.Load())
}

func TestCodeServerOtherVersions(t *testing.T) {
	const older = "code-server-4.143.0-linux-amd64.tar.gz"
	// The configured checksum applies to the configured version only.
	cst := newCodeServerTest(t, nil, func(c *config.CodeServerConfig) {
		c.SHA256 = map[string]string{"amd64": testCodeServerSum("arm64")}
	})
	placeCodeServerFile(t, filepath.Join(cst.dir, older), testCodeServerContent("amd64"), 1)

	rec := cst.get("/det/code-server/4.143.0/" + older)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, testCodeServerContent("amd64"), rec.Body.Bytes())
	rec = cst.get("/det/code-server/4.143.0/" + older + ".sha256")
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, testCodeServerSum("amd64")+"  "+older+"\n", rec.Body.String())
	require.Equal(t, []string{older, older + ".sha256"}, cst.files(t))

	// A version other than the configured one is never downloaded.
	for _, p := range []string{
		"4.143.0/code-server-4.143.0-linux-arm64.tar.gz",
		"4.143.0/code-server-4.143.0-linux-arm64.tar.gz.sha256",
		"4.145.0/code-server-4.145.0-linux-amd64.tar.gz",
	} {
		require.Equal(t, http.StatusNotFound, cst.get("/det/code-server/"+p).Code, p)
	}
	require.Equal(t, int32(0), cst.calls.Load())
	require.Equal(t, []string{older, older + ".sha256"}, cst.files(t))
}
