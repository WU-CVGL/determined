package config

import (
	"strings"
	"testing"

	"github.com/ghodss/yaml"
	"github.com/stretchr/testify/require"

	"github.com/determined-ai/determined/master/pkg/check"
)

const testCodeServerSHA256 = "c0ffee0000000000000000000000000000000000000000000000000000000000"

func TestCodeServerConfigDefaults(t *testing.T) {
	conf := DefaultConfig()
	require.False(t, conf.Interactive.CodeServer.Enabled())
	require.Equal(t, DefaultCodeServerURLTemplate, conf.Interactive.CodeServer.URLTemplate)
	require.NoError(t, check.Validate(conf.Interactive))

	require.NoError(t, yaml.Unmarshal([]byte(`
interactive:
  code_server:
    version: 4.144.0
    dir: /var/cache/determined/code-server
`), conf, yaml.DisallowUnknownFields))
	cs := conf.Interactive.CodeServer
	require.True(t, cs.Enabled())
	require.NoError(t, check.Validate(conf.Interactive))
	require.Equal(t, DefaultCodeServerURLTemplate, cs.URLTemplate)
	require.Equal(t, "https://github.com/coder/code-server/releases/download/v4.144.0/"+
		"code-server-4.144.0-linux-arm64.tar.gz", cs.URL("arm64"))
}

func TestCodeServerConfigValidate(t *testing.T) {
	valid := func() CodeServerConfig {
		return CodeServerConfig{
			Version:     "4.144.0",
			Dir:         "/var/cache/determined/code-server",
			URLTemplate: DefaultCodeServerURLTemplate,
		}
	}
	tests := []struct {
		name   string
		modify func(*CodeServerConfig)
		err    string
	}{
		{"valid", func(*CodeServerConfig) {}, ""},
		{"mirror", func(c *CodeServerConfig) {
			c.URLTemplate = "http://mirror.example:8081/code-server/{version}/{arch}.tar.gz"
		}, ""},
		{"checksums", func(c *CodeServerConfig) {
			c.SHA256 = map[string]string{"amd64": testCodeServerSHA256, "arm64": strings.ToUpper(testCodeServerSHA256)}
		}, ""},
		{"missing version", func(c *CodeServerConfig) { c.Version = "" }, "version"},
		{"bad version", func(c *CodeServerConfig) { c.Version = "v4.144.0" }, "version"},
		{"prerelease version", func(c *CodeServerConfig) { c.Version = "4.144.0-rc.1" }, "version"},
		{"path in version", func(c *CodeServerConfig) { c.Version = "4.144.0/../x" }, "version"},
		{"missing dir", func(c *CodeServerConfig) { c.Dir = "" }, "dir"},
		{"relative dir", func(c *CodeServerConfig) { c.Dir = "cache/code-server" }, "dir"},
		{"only a checksum", func(c *CodeServerConfig) {
			*c = CodeServerConfig{
				Dir:         c.Dir,
				URLTemplate: DefaultCodeServerURLTemplate,
				SHA256:      map[string]string{"amd64": testCodeServerSHA256},
			}
		}, "version"},
		{"empty url_template", func(c *CodeServerConfig) { c.URLTemplate = "" }, "url_template"},
		{"no arch placeholder", func(c *CodeServerConfig) {
			c.URLTemplate = "https://mirror.example/code-server-{version}.tar.gz"
		}, "url_template"},
		{"no version placeholder", func(c *CodeServerConfig) {
			c.URLTemplate = "https://mirror.example/code-server-{arch}.tar.gz"
		}, "url_template"},
		{"file url_template", func(c *CodeServerConfig) {
			c.URLTemplate = "file:///srv/code-server-{version}-linux-{arch}.tar.gz"
		}, "url_template"},
		{"no host", func(c *CodeServerConfig) { c.URLTemplate = "https:///{version}/{arch}.tar.gz" }, "url_template"},
		{"credentials in url_template", func(c *CodeServerConfig) {
			c.URLTemplate = "https://user:secret@mirror.example/{version}/{arch}.tar.gz"
		}, "credentials"},
		{"unknown arch", func(c *CodeServerConfig) {
			c.SHA256 = map[string]string{"armhf": testCodeServerSHA256}
		}, "unknown architecture"},
		{"short checksum", func(c *CodeServerConfig) {
			c.SHA256 = map[string]string{"amd64": testCodeServerSHA256[1:]}
		}, "sha256.amd64"},
		{"non-hex checksum", func(c *CodeServerConfig) {
			c.SHA256 = map[string]string{"arm64": "g" + testCodeServerSHA256[1:]}
		}, "sha256.arm64"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			conf := valid()
			test.modify(&conf)
			errs := conf.Validate()
			if test.err == "" {
				require.Empty(t, errs)
				return
			}
			require.Len(t, errs, 1, "%v", errs)
			require.ErrorContains(t, errs[0], test.err)
		})
	}
	// check.Validate, which validates the master config, reaches the section.
	require.ErrorContains(t, check.Validate(InteractiveConfig{CodeServer: CodeServerConfig{
		Version: "4.144.0", URLTemplate: DefaultCodeServerURLTemplate,
	}}), "interactive.code_server.dir")
}
