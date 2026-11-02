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
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/hashicorp/go-cleanhttp"
	"github.com/labstack/echo/v4"
	log "github.com/sirupsen/logrus"
	"golang.org/x/sync/singleflight"

	"github.com/determined-ai/determined/master/internal/config"
	detContext "github.com/determined-ai/determined/master/internal/context"
	"github.com/determined-ai/determined/master/internal/db"
	"github.com/determined-ai/determined/master/internal/grpcutil"
	"github.com/determined-ai/determined/master/internal/user"
	"github.com/determined-ai/determined/master/pkg/model"
)

const (
	// codeServerRoute serves code-server release tarballs to tasks. It is under /det/, which the
	// authentication middleware exempts for the WebUI, so the route authenticates by itself, with
	// a user session or the token of an allocation. The wildcard keeps every other path below
	// /det/code-server/ away from the WebUI's catch-all.
	codeServerRoute = "/det/code-server/*"
	// codeServerFetchTimeout bounds one download of a tarball, about 230 MB, from upstream.
	codeServerFetchTimeout = 15 * time.Minute
)

var codeServerFilePattern = regexp.MustCompile(
	`^code-server-([0-9]+\.[0-9]+\.[0-9]+)-linux-([a-z0-9]+)\.tar\.gz(\.sha256)?$`)

var (
	errCodeServerUpstream = errors.New("code-server download failed")
	errCodeServerChecksum = errors.New("code-server checksum mismatch")
	// errCodeServerMissing is the error for a tarball of a version other than conf.Version that is
	// not in conf.Dir.
	errCodeServerMissing = errors.New("code-server tarball not found")
)

// codeServerArtifacts serves the code-server tarballs in conf.Dir, downloading each one of the
// configured version on the first request for it.
type codeServerArtifacts struct {
	conf   config.CodeServerConfig
	client *http.Client //nolint:forbidigo // Built by cleanhttp.
	// fetches lets concurrent requests for the files of one version and architecture share one
	// download or hash.
	fetches singleflight.Group

	// mu guards sums.
	mu sync.Mutex
	// sums has the SHA-256 of each tarball in conf.Dir by its path, with the identity of the file
	// that was hashed.
	sums map[string]codeServerSum
	// hashed counts the tarballs read to compute their SHA-256.
	hashed atomic.Int32
}

// codeServerSum is the SHA-256 of a file with the identity of that file.
type codeServerSum struct {
	id  codeServerFileID
	sum string
}

// codeServerFileID identifies a file and its content: replacing a file changes its device or
// inode, and writing to it or setting its times changes its status change time, which no one can
// set.
type codeServerFileID struct {
	size  int64
	mtime int64
	dev   uint64
	inode uint64
	ctime int64
}

func codeServerFileIDOf(info fs.FileInfo) codeServerFileID {
	id := codeServerFileID{size: info.Size(), mtime: info.ModTime().UnixNano()}
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		id.dev, id.inode, id.ctime = codeServerStatID(st)
	}
	return id
}

func newCodeServerArtifacts(conf config.CodeServerConfig) (*codeServerArtifacts, error) {
	if err := os.MkdirAll(conf.Dir, 0o755); err != nil { //nolint:gosec // Tasks read the files.
		return nil, fmt.Errorf("creating interactive.code_server.dir: %w", err)
	}
	// cleanhttp's transport takes HTTPS_PROXY and NO_PROXY from the master's environment.
	client := cleanhttp.DefaultClient()
	client.Timeout = codeServerFetchTimeout
	return &codeServerArtifacts{
		conf: conf, client: client, sums: map[string]codeServerSum{},
	}, nil
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
	e.GET(codeServerRoute, artifacts.serve, codeServerAuthentication)
	return nil
}

// codeServerAuthentication lets a request pass with a user session, like
// user.Service.RequireAuthentication, or with the token of an allocation, its DET_SESSION_TOKEN. A
// task's user session can end before the task starts; its allocation session lasts as long as the
// allocation.
func codeServerAuthentication(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		userErr := user.GetService().Authenticate(c)
		if userErr == nil {
			return next(c)
		}
		var session *model.AllocationSession
		for _, token := range codeServerAllocationTokens(c.Request()) {
			var err error
			if session, err = grpcutil.AllocationSessionByToken(token); err == nil {
				break
			} else if !errors.Is(err, db.ErrNotFound) {
				return err
			}
		}
		switch {
		case session == nil:
			return userErr
		case session.OwnerID == nil:
			return echo.NewHTTPError(http.StatusUnauthorized)
		}
		owner, err := user.ByID(c.Request().Context(), *session.OwnerID)
		switch {
		case errors.Is(err, db.ErrNotFound):
			return echo.NewHTTPError(http.StatusUnauthorized)
		case err != nil:
			return err
		case !owner.Active:
			return echo.NewHTTPError(http.StatusForbidden, "user not active")
		}
		c.(*detContext.DetContext).SetUser(owner.ToUser())
		return next(c)
	}
}

// codeServerAllocationTokens returns the bearer tokens of the request's Authorization header and
// of its x-allocation-token header, which the Determined Python client sends as gRPC metadata.
func codeServerAllocationTokens(r *http.Request) []string {
	var tokens []string
	for _, header := range []string{
		echo.HeaderAuthorization,
		grpcutil.AllocationTokenHeader,
		grpcutil.GrpcMetadataPrefix + grpcutil.AllocationTokenHeader,
	} {
		if token, ok := strings.CutPrefix(r.Header.Get(header), "Bearer "); ok && token != "" {
			tokens = append(tokens, token)
		}
	}
	return tokens
}

func codeServerTarball(version, arch string) string {
	return "code-server-" + version + "-linux-" + arch + ".tar.gz"
}

// match returns the version and the architecture, one of config.CodeServerArches, that a request
// path below codeServerRoute names, and whether it asks for the checksum file. ok is false for
// every path other than <version>/<tarball> and <version>/<tarball>.sha256 of the same version.
func (a *codeServerArtifacts) match(path string) (version, arch string, checksum, ok bool) {
	version, file, found := strings.Cut(path, "/")
	if !found {
		return "", "", false, false
	}
	m := codeServerFilePattern.FindStringSubmatch(file)
	if m == nil || m[1] != version {
		return "", "", false, false
	}
	for _, known := range config.CodeServerArches {
		if m[2] == known {
			return m[1], known, m[3] != "", true
		}
	}
	return "", "", false, false
}

func (a *codeServerArtifacts) serve(c echo.Context) error {
	if a == nil {
		return echo.NewHTTPError(http.StatusNotFound)
	}
	// The served paths need no escaping, so an escaped path, such as one with %2F, is not one.
	if c.Request().URL.RawPath != "" {
		return echo.NewHTTPError(http.StatusNotFound)
	}
	version, arch, checksum, ok := a.match(c.Param("*"))
	if !ok {
		return echo.NewHTTPError(http.StatusNotFound)
	}
	// The download does not depend on the requests that wait for it, so a client that goes away
	// does not cancel it for the others.
	_, err, _ := a.fetches.Do(version+"/"+arch, func() (interface{}, error) {
		return nil, a.prepare(version, arch)
	})
	// The response names the failure only; the log has the URL and the checksums.
	switch {
	case errors.Is(err, errCodeServerMissing):
		return echo.NewHTTPError(http.StatusNotFound)
	case errors.Is(err, errCodeServerUpstream):
		log.WithError(err).Warnf("serving code-server %s for %s", version, arch)
		return echo.NewHTTPError(http.StatusBadGateway, errCodeServerUpstream.Error())
	case errors.Is(err, errCodeServerChecksum):
		log.WithError(err).Warnf("serving code-server %s for %s", version, arch)
		return echo.NewHTTPError(http.StatusBadGateway, errCodeServerChecksum.Error())
	case err != nil:
		log.WithError(err).Errorf("serving code-server %s for %s", version, arch)
		return echo.NewHTTPError(http.StatusInternalServerError)
	}

	name, contentType := codeServerTarball(version, arch), "application/gzip"
	if checksum {
		name, contentType = name+".sha256", "text/plain; charset=utf-8"
	}
	f, err := os.Open(filepath.Join(a.conf.Dir, name)) //nolint:gosec // name is built by match.
	if errors.Is(err, fs.ErrNotExist) {
		// Removed since prepare.
		return echo.NewHTTPError(http.StatusNotFound)
	} else if err != nil {
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

// prepare makes sure that the tarball of version for arch and its checksum file are in conf.Dir,
// and that a tarball of conf.Version matches conf.SHA256. It downloads a missing tarball of
// conf.Version only; for other versions it returns errCodeServerMissing. It hashes a tarball that
// it has not hashed before or that has changed since, and rewrites a checksum file that does not
// match. It never removes a tarball that it did not download.
func (a *codeServerArtifacts) prepare(version, arch string) error {
	path := filepath.Join(a.conf.Dir, codeServerTarball(version, arch))
	info, err := os.Stat(path)
	switch {
	case err != nil && version != a.conf.Version:
		// A tarball that cannot be found, such as one whose name is too long for the filesystem,
		// is not in conf.Dir.
		return errCodeServerMissing
	case errors.Is(err, fs.ErrNotExist):
		return a.download(arch, path)
	case err != nil:
		return err
	case !info.Mode().IsRegular():
		return fmt.Errorf("%s is not a regular file", path)
	}

	sum, err := a.sum(path, info)
	if err != nil {
		return err
	}
	if version == a.conf.Version {
		if err := a.verify(arch, sum); err != nil {
			return err
		}
	}
	if b, err := os.ReadFile(path + ".sha256"); err == nil && //nolint:gosec // Built by match.
		bytes.Equal(b, checksumLine(path, sum)) {
		return nil
	}
	return writeChecksumFile(path, sum)
}

// sum returns the SHA-256 of the tarball at path, whose information is info, hashing it unless it
// has hashed the same file before.
func (a *codeServerArtifacts) sum(path string, info fs.FileInfo) (string, error) {
	a.mu.Lock()
	cached, ok := a.sums[path]
	a.mu.Unlock()
	if ok && cached.id == codeServerFileIDOf(info) {
		return cached.sum, nil
	}

	f, err := os.Open(path) //nolint:gosec // path is built by match.
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	// The identity of the opened file, which is the one hashed even if path is replaced meanwhile.
	if info, err = f.Stat(); err != nil {
		return "", err
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	a.hashed.Add(1)
	// The sum of a file written to while it was read matches no content of it.
	after, err := f.Stat()
	if err != nil {
		return "", err
	}
	if codeServerFileIDOf(after) != codeServerFileIDOf(info) {
		return "", fmt.Errorf("%s changed while it was hashed", path)
	}
	sum := hex.EncodeToString(h.Sum(nil))
	a.remember(path, info, sum)
	return sum, nil
}

func (a *codeServerArtifacts) remember(path string, info fs.FileInfo, sum string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.sums[path] = codeServerSum{id: codeServerFileIDOf(info), sum: sum}
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
	rawURL := a.conf.URL(arch)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return fmt.Errorf("%w: invalid URL %s", errCodeServerUpstream, redactURL(rawURL))
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return codeServerUpstreamError(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%w: %s answered %s", errCodeServerUpstream, redactURL(rawURL), resp.Status)
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
		return codeServerUpstreamError(err)
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
	if info, err := os.Stat(path); err == nil {
		a.remember(path, info, sum)
	}
	return writeChecksumFile(path, sum)
}

// codeServerUpstreamError wraps an error of the HTTP client, naming the URL in its redacted form.
func codeServerUpstreamError(err error) error {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return fmt.Errorf("%w: %s %s: %w", errCodeServerUpstream, urlErr.Op, redactURL(urlErr.URL),
			urlErr.Err)
	}
	return fmt.Errorf("%w: %w", errCodeServerUpstream, err)
}

// redactURL returns the scheme, host and path of raw, without credentials in its userinfo or its
// query, for logs and errors.
func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "(invalid URL)"
	}
	return (&url.URL{Scheme: u.Scheme, Host: u.Host, Path: u.Path, RawPath: u.RawPath}).String()
}

// checksumLine is a line of sha256sum's output for the file at path.
func checksumLine(path, sum string) []byte {
	return []byte(sum + "  " + filepath.Base(path) + "\n")
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
