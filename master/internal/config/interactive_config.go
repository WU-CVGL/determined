package config

import (
	"fmt"
	"net/url"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// DefaultCodeServerURLTemplate is where the master downloads code-server release tarballs from.
const DefaultCodeServerURLTemplate = "https://github.com/coder/code-server/releases/download/" +
	"v{version}/code-server-{version}-linux-{arch}.tar.gz"

// CodeServerArches are the architectures whose code-server tarballs the master serves.
var CodeServerArches = []string{"amd64", "arm64"}

var (
	codeServerVersionPattern = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)
	sha256HexPattern         = regexp.MustCompile(`^[0-9a-fA-F]{64}$`)
)

// InteractiveConfig configures what the master provides to interactive tasks.
type InteractiveConfig struct {
	CodeServer CodeServerConfig `json:"code_server"`
}

// CodeServerConfig configures the code-server release tarballs that the master serves to tasks.
// The master downloads a tarball into Dir on the first request for it, unless an administrator
// has placed it there. An unset section turns the feature off.
type CodeServerConfig struct {
	// Version is the one code-server release that the master serves.
	Version string `json:"version"`
	// Dir is the absolute path of the directory that holds the tarballs and their checksums.
	Dir string `json:"dir"`
	// URLTemplate is the download URL, with the placeholders {version} and {arch}.
	URLTemplate string `json:"url_template"`
	// SHA256 maps an architecture to the expected SHA-256 of its tarball, in hex.
	SHA256 map[string]string `json:"sha256"`
}

// DefaultCodeServerConfig returns the defaults of the code_server section.
func DefaultCodeServerConfig() CodeServerConfig {
	return CodeServerConfig{URLTemplate: DefaultCodeServerURLTemplate}
}

// Enabled returns whether the master serves code-server.
func (c CodeServerConfig) Enabled() bool { return c.Version != "" && c.Dir != "" }

// isSet returns whether any key of the section differs from its default.
func (c CodeServerConfig) isSet() bool {
	return c.Version != "" || c.Dir != "" || len(c.SHA256) > 0 ||
		c.URLTemplate != DefaultCodeServerURLTemplate
}

// URL returns the download URL of the tarball for arch.
func (c CodeServerConfig) URL(arch string) string {
	return strings.NewReplacer("{version}", c.Version, "{arch}", arch).Replace(c.URLTemplate)
}

// Validate implements the check.Validatable interface.
func (c CodeServerConfig) Validate() []error {
	if !c.isSet() {
		return nil
	}
	var errs []error
	if !codeServerVersionPattern.MatchString(c.Version) {
		errs = append(errs, fmt.Errorf(
			"interactive.code_server.version must be a release number such as 4.144.0"))
	}
	if c.Dir == "" || !filepath.IsAbs(c.Dir) {
		errs = append(errs, fmt.Errorf("interactive.code_server.dir must be an absolute path"))
	}
	if !strings.Contains(c.URLTemplate, "{version}") || !strings.Contains(c.URLTemplate, "{arch}") {
		errs = append(errs, fmt.Errorf(
			"interactive.code_server.url_template must contain {version} and {arch}"))
	} else if u, err := url.Parse(c.URL(CodeServerArches[0])); err != nil ||
		(u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		errs = append(errs, fmt.Errorf("interactive.code_server.url_template must be an http(s) URL"))
	} else if u.User != nil {
		// The master prints its configuration and the download URL to its log.
		errs = append(errs, fmt.Errorf(
			"interactive.code_server.url_template must not contain credentials"))
	}
	arches := make([]string, 0, len(c.SHA256))
	for arch := range c.SHA256 {
		arches = append(arches, arch)
	}
	sort.Strings(arches)
	for _, arch := range arches {
		if !IsCodeServerArch(arch) {
			errs = append(errs, fmt.Errorf(
				"interactive.code_server.sha256: unknown architecture %q, expected one of %s",
				arch, strings.Join(CodeServerArches, ", ")))
		} else if !sha256HexPattern.MatchString(c.SHA256[arch]) {
			errs = append(errs, fmt.Errorf(
				"interactive.code_server.sha256.%s must be 64 hexadecimal characters", arch))
		}
	}
	return errs
}

// IsCodeServerArch returns whether the master serves code-server for arch.
func IsCodeServerArch(arch string) bool {
	for _, a := range CodeServerArches {
		if a == arch {
			return true
		}
	}
	return false
}
