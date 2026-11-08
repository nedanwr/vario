package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func valid() *Config {
	c := Default()
	c.ClickHouse.URL = "https://ch.example.com:8443"
	c.Collector.DataDir = "/tmp/vario"
	return c
}

func TestDefaultsValidate(t *testing.T) {
	if err := valid().Validate(); err != nil {
		t.Fatalf("defaults should validate: %v", err)
	}
	if Default().Privacy.Mode != ModeMetrics || !Default().Privacy.RedactSecrets {
		t.Fatal("defaults must be conservative")
	}
}

func TestValidateRejects(t *testing.T) {
	cases := map[string]struct {
		mutate func(*Config)
		want   string
	}{
		"sensitive mode without opt-in": {func(c *Config) { c.Privacy.Mode = ModeFull }, "allow_sensitive_content"},
		"unknown mode":                  {func(c *Config) { c.Privacy.Mode = "everything" }, "privacy.mode"},
		"remote http":                   {func(c *Config) { c.ClickHouse.URL = "http://ch.example.com:8123" }, "unencrypted"},
		"credentials in url":            {func(c *Config) { c.ClickHouse.URL = "https://u:p@ch.example.com" }, "must not embed credentials"},
		"bad scheme":                    {func(c *Config) { c.ClickHouse.URL = "tcp://ch:9000" }, "http or https"},
		"missing url":                   {func(c *Config) { c.ClickHouse.URL = "" }, "clickhouse.url is required"},
		"bad database":                  {func(c *Config) { c.ClickHouse.Database = "x; DROP" }, "plain identifier"},
		"unsupported source":            {func(c *Config) { c.Sources = []SourceConfig{{Kind: "chatgpt"}} }, "not supported"},
		"duplicate source": {func(c *Config) {
			c.Sources = []SourceConfig{{Kind: SourceT3Code}, {Kind: SourceT3Code}}
		}, "more than once"},
		"bad pattern":   {func(c *Config) { c.Privacy.ExtraPatterns = []string{"("} }, "invalid pattern"},
		"tiny poll":     {func(c *Config) { c.Collector.PollInterval.Duration = 1 }, "poll_interval"},
		"tiny spool":    {func(c *Config) { c.Spool.MaxSizeMB = 1 }, "max_size_mb"},
		"bad log level": {func(c *Config) { c.Collector.LogLevel = "loud" }, "log_level"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			c := valid()
			tc.mutate(c)
			err := c.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want error containing %q, got %v", tc.want, err)
			}
		})
	}
}

func TestValidateAllows(t *testing.T) {
	c := valid()
	c.ClickHouse.URL = "http://localhost:8123"
	c.Privacy.Mode = ModeMessages
	c.Privacy.AllowSensitiveContent = true
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	c.ClickHouse.URL = "http://10.0.0.5:8123"
	c.ClickHouse.AllowInsecureHTTP = true
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	c := valid()
	c.SetPath(filepath.Join(dir, "sub", "config.toml"))
	c.Privacy.ExtraPatterns = []string{`acme-[0-9]+`, `quote"and\back`}
	c.UpsertSource(SourceConfig{Kind: SourceT3Code, Enabled: true, Path: filepath.Join(dir, "has space", "statev2.sqlite")})
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		st, _ := os.Stat(c.Path())
		if st.Mode().Perm() != 0o600 {
			t.Fatalf("config mode %o", st.Mode().Perm())
		}
	}
	got, err := Load(c.Path())
	if err != nil {
		t.Fatal(err)
	}
	if err := got.Validate(); err != nil {
		t.Fatal(err)
	}
	if got.ClickHouse.URL != c.ClickHouse.URL || got.Collector.PollInterval != c.Collector.PollInterval ||
		len(got.Privacy.ExtraPatterns) != 2 || got.Privacy.ExtraPatterns[1] != `quote"and\back` {
		t.Fatalf("round trip mismatch: %+v", got)
	}
	sc, ok := got.Source(SourceT3Code)
	if !ok || sc.Path != c.Sources[0].Path || !sc.Enabled {
		t.Fatalf("source round trip: %+v", sc)
	}
}

func TestLoadRejectsUnknownKeys(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(p, []byte("[clickhouse]\nurl = \"https://x\"\npasword = \"typo\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p); err == nil || !strings.Contains(err.Error(), "pasword") {
		t.Fatalf("expected unknown key error, got %v", err)
	}
	if _, err := Load(filepath.Join(t.TempDir(), "missing.toml")); err == nil {
		t.Fatal("expected not found")
	}
}

func TestPasswordSources(t *testing.T) {
	c := valid()
	c.SetPath(filepath.Join(t.TempDir(), "config.toml"))
	if pw, err := c.Password(); err != nil || pw != "" {
		t.Fatalf("missing password file should yield empty password: %q %v", pw, err)
	}
	if err := c.WritePassword("s3cret"); err != nil {
		t.Fatal(err)
	}
	if pw, _ := c.Password(); pw != "s3cret" {
		t.Fatalf("password = %q", pw)
	}
	t.Setenv(EnvPassword, "from-env")
	if pw, _ := c.Password(); pw != "from-env" {
		t.Fatalf("env should take precedence, got %q", pw)
	}
}

func TestExpandPath(t *testing.T) {
	home, _ := os.UserHomeDir()
	got, err := ExpandPath("~/x/y")
	if err != nil || got != filepath.Join(home, "x", "y") {
		t.Fatalf("ExpandPath = %q %v", got, err)
	}
}

func TestExampleConfigIsValid(t *testing.T) {
	c, err := Load(filepath.Join("..", "..", "config.example.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
}
