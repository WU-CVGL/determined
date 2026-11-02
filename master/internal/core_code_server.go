package internal

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/hashicorp/go-cleanhttp"
	"github.com/labstack/echo/v4"
	log "github.com/sirupsen/logrus"
	"golang.org/x/sync/singleflight"

	"github.com/determined-ai/determined/master/internal/config"
	"github.com/determined-ai/determined/master/internal/user"
)

const (
	// codeServerRoute serves code-server release tarballs to tasks. It is under /det/, which the
	// authentication middleware exempts for the WebUI, so the route authenticates by itself. The
	// wildcard keeps every other path below /det/code-server/ away from the WebUI's catch-all.
	codeServerRoute = "/det/code-server/*"
	// codeServerFetchTimeout bounds one download of a tarball, about 230 MB, from upstream.
	codeServerFetchTimeout = 15 * time.Minute
)

var codeServerFilePattern = regexp.MustCompile(
	`^code-server-([0-9]+\.[0-9]+\.[0-9]+)-linux-([a-z0-9]+)\.tar\.gz(\.sha256)?$`)

var (
	errCodeServerUpstream = errors.New("code-server download failed")
	errCodeServerChecksum = errors.New("code-server checksum mismatch")
)

// codeServerArtifacts serves the tarballs of the configured code-server version from conf.Dir,
// downloading each one on the first request for it.
type codeServerArtifacts struct {
	conf   config.CodeServerConfig
	client *http.Client //nolint:forbidigo // Built by cleanhttp.
	// fetches lets concurrent requests for the files of one architecture share one download.
	fetches singleflight.Group
}

func newCodeServerArtifacts(conf config.CodeServerConfig) (*codeServerArtifacts, error) {
	if err := os.MkdirAll(conf.Dir, 0o755); err != nil { //nolint:gosec // Tasks read the files.
		return nil, fmt.Errorf("creating interactive.code_server.dir: %w", err)
	}
	// cleanhttp's transport takes HTTPS_PROXY and NO_PROXY from the master's environment.
	client := cleanhttp.DefaultClient()
	client.Timeout = codeServerFetchTimeout
	return &codeServerArtifacts{conf: conf, client: client}, nil
}

// registerCodeServerRoutes registers the code-server route. Without the interactive.code_server
// section, the route answers 404 to every authenticated request.
func registerCodeServerRoutes(e *echo.Echo, conf config.CodeServerConfig) error {
	var artifacts *codeServerArtifacts
	if conf.Enabled() {
		var err error
		if artifacts, err = newCodeServerArtifacts(conf); err != nil {
			return err
		}
	}
	e.GET(codeServerRoute, artifacts.serve, user.GetService().RequireAuthentication)
	return nil
}

func codeServerTarball(version, arch string) string {
	return "code-server-" + version + "-linux-" + arch + ".tar.gz"
}

// match returns the architecture that a request path below codeServerRoute names, one of
// config.CodeServerArches, and whether it asks for the checksum file. ok is false for every
// path other than <version>/<tarball> and <version>/<tarball>.sha256 of the configured version.
func (a *codeServerArtifacts) match(path string) (arch string, checksum bool, ok bool) {
	version, file, found := strings.Cut(path, "/")
	if !found || version != a.conf.Version {
		return "", false, false
	}
	m := codeServerFilePattern.FindStringSubmatch(file)
	if m == nil || m[1] != a.conf.Version {
		return "", false, false
	}
	for _, known := range config.CodeServerArches {
		if m[2] == known {
			return known, m[3] != "", true
		}
	}
	return "", false, false
}

func (a *codeServerArtifacts) serve(c echo.Context) error {
	if a == nil {
		return echo.NewHTTPError(http.StatusNotFound)
	}
	// The served paths need no escaping, so an escaped path, such as one with %2F, is not one.
	if c.Request().URL.RawPath != "" {
		return echo.NewHTTPError(http.StatusNotFound)
	}
	arch, checksum, ok := a.match(c.Param("*"))
	if !ok {
		return echo.NewHTTPError(http.StatusNotFound)
	}
	// The download does not depend on the requests that wait for it, so a client that goes away
	// does not cancel it for the others.
	_, err, _ := a.fetches.Do(arch, func() (interface{}, error) { return nil, a.prepare(arch) })
	// The response names the failure only; the log has the URL and the checksums.
	switch {
	case errors.Is(err, errCodeServerUpstream):
		log.WithError(err).Warnf("serving code-server %s for %s", a.conf.Version, arch)
		return echo.NewHTTPError(http.StatusBadGateway, errCodeServerUpstream.Error())
	case errors.Is(err, errCodeServerChecksum):
		log.WithError(err).Warnf("serving code-server %s for %s", a.conf.Version, arch)
		return echo.NewHTTPError(http.StatusBadGateway, errCodeServerChecksum.Error())
	case err != nil:
		log.WithError(err).Errorf("serving code-server %s for %s", a.conf.Version, arch)
		return echo.NewHTTPError(http.StatusInternalServerError)
	}

	name, contentType := codeServerTarball(a.conf.Version, arch), "application/gzip"
	if checksum {
		name, contentType = name+".sha256", "text/plain; charset=utf-8"
	}
	f, err := os.Open(filepath.Join(a.conf.Dir, name)) //nolint:gosec // name is built from config.
	if err != nil {
		return fmt.Errorf("opening %s: %w", name, err)
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("reading %s: %w", name, err)
	}
	c.Response().Header().Set("Content-Type", contentType)
	http.ServeContent(c.Response(), c.Request(), name, info.ModTime(), f)
	return nil
}

// prepare makes sure that the tarball for arch and its checksum file are in conf.Dir and that the
// tarball matches conf.SHA256, downloading the tarball if it is missing. It never removes a
// tarball that it did not download.
func (a *codeServerArtifacts) prepare(arch string) error {
	path := filepath.Join(a.conf.Dir, codeServerTarball(a.conf.Version, arch))
	info, err := os.Stat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return a.download(arch, path)
	case err != nil:
		return err
	case !info.Mode().IsRegular():
		return fmt.Errorf("%s is not a regular file", path)
	}

	sum, err := readChecksumFile(path)
	if err != nil {
		if sum, err = hashFile(path); err != nil {
			return err
		}
		if err := a.verify(arch, sum); err != nil {
			return err
		}
		return writeChecksumFile(path, sum)
	}
	return a.verify(arch, sum)
}

func (a *codeServerArtifacts) verify(arch, sum string) error {
	if want, ok := a.conf.SHA256[arch]; ok && !strings.EqualFold(want, sum) {
		return fmt.Errorf("%w: %s has SHA-256 %s", errCodeServerChecksum,
			codeServerTarball(a.conf.Version, arch), sum)
	}
	return nil
}

// download streams the tarball for arch into <path>.tmp while hashing it and renames it to path
// once it is complete and matches conf.SHA256. On failure it leaves nothing in conf.Dir.
func (a *codeServerArtifacts) download(arch, path string) (err error) {
	ctx, cancel := context.WithTimeout(context.Background(), codeServerFetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.conf.URL(arch), nil)
	if err != nil {
		return fmt.Errorf("%w: %w", errCodeServerUpstream, err)
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %w", errCodeServerUpstream, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%w: %s answered %s", errCodeServerUpstream, a.conf.URL(arch), resp.Status)
	}

	// A .tmp file left by a master that stopped during a download is overwritten.
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644) //nolint:gosec
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = os.Remove(tmp)
		}
	}()
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(f, h), resp.Body); err != nil {
		_ = f.Close()
		return fmt.Errorf("%w: %w", errCodeServerUpstream, err)
	}
	if err := syncAndClose(f); err != nil {
		return err
	}
	sum := hex.EncodeToString(h.Sum(nil))
	if err := a.verify(arch, sum); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	log.Infof("downloaded %s, SHA-256 %s", filepath.Base(path), sum)
	return writeChecksumFile(path, sum)
}

func hashFile(path string) (string, error) {
	f, err := os.Open(path) //nolint:gosec // path is built from config.
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// checksumLine is a line of sha256sum's output for the file at path.
func checksumLine(path, sum string) []byte {
	return []byte(sum + "  " + filepath.Base(path) + "\n")
}

// readChecksumFile returns the checksum in <path>.sha256, or an error if the file is missing or
// is not sha256sum's line for path.
func readChecksumFile(path string) (string, error) {
	b, err := os.ReadFile(path + ".sha256") //nolint:gosec // path is built from config.
	if err != nil {
		return "", err
	}
	sum, _, _ := bytes.Cut(b, []byte("  "))
	if len(sum) != sha256.Size*2 || !bytes.Equal(b, checksumLine(path, string(sum))) {
		return "", fmt.Errorf("%s.sha256 is not a checksum line for %s", path, filepath.Base(path))
	}
	if _, err := hex.DecodeString(string(sum)); err != nil {
		return "", fmt.Errorf("%s.sha256: %w", path, err)
	}
	return string(sum), nil
}

// writeChecksumFile writes <path>.sha256 in sha256sum's format, through a temporary file so that
// a reader never sees a partial line. It then syncs the directory, which makes the renames of the
// tarball and of its checksum file durable.
func writeChecksumFile(path, sum string) error {
	tmp := path + ".sha256.tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644) //nolint:gosec
	if err != nil {
		return err
	}
	if _, err := f.Write(checksumLine(path, sum)); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := syncAndClose(f); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path+".sha256"); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return syncDir(filepath.Dir(path))
}

// syncAndClose flushes f to disk and closes it. A file is synced before it is renamed into place,
// so that after a crash the new name never refers to a truncated file.
func syncAndClose(f *os.File) error {
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

func syncDir(dir string) error {
	d, err := os.Open(dir) //nolint:gosec // dir is from config.
	if err != nil {
		return err
	}
	return syncAndClose(d)
}
