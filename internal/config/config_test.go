package config

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
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

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func setHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	for _, k := range []string{EnvConfig, EnvHome} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
	return home
}

func TestResolvePrecedence(t *testing.T) {
	home := setHome(t)
	std := filepath.Join(home, ".vario", "config.toml")
	loc, err := Resolve("")
	if err != nil || loc.Path != std || loc.Origin != FromDefault {
		t.Fatalf("default: %+v %v", loc, err)
	}
	alt := filepath.Join(home, "alt")
	t.Setenv(EnvHome, alt)
	if loc, _ := Resolve(""); loc.Path != filepath.Join(alt, "config.toml") || loc.Origin != FromHome {
		t.Fatalf("$VARIO_HOME: %+v", loc)
	}
	t.Setenv(EnvConfig, "~/env.toml")
	if loc, _ := Resolve(""); loc.Path != filepath.Join(home, "env.toml") || loc.Origin != FromEnv {
		t.Fatalf("$VARIO_CONFIG: %+v", loc)
	}
	if loc, _ := Resolve("rel/flag.toml"); !filepath.IsAbs(loc.Path) || loc.Origin != FromFlag {
		t.Fatalf("--config: %+v", loc)
	}
}

func TestDefaultDataDir(t *testing.T) {
	home := setHome(t)
	d, err := DefaultDataDir()
	if err != nil || d != filepath.Join(home, ".vario", "data") {
		t.Fatalf("default data dir: %q %v", d, err)
	}
	t.Setenv(EnvHome, filepath.Join(home, "iso"))
	if d, _ := DefaultDataDir(); d != filepath.Join(home, "iso", "data") {
		t.Fatalf("$VARIO_HOME data dir: %q", d)
	}
}

func TestLoadErrors(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.toml")
	os.WriteFile(bad, []byte("[clickhouse\nurl="), 0o600)
	if _, err := Load(bad); err == nil || errors.Is(err, ErrNotFound) || !strings.Contains(err.Error(), "parse") {
		t.Fatalf("malformed: %v", err)
	}
	if runtime.GOOS != "windows" && os.Getuid() != 0 {
		locked := filepath.Join(dir, "locked.toml")
		os.WriteFile(locked, []byte(minimal), 0o000)
		if _, err := Load(locked); err == nil || errors.Is(err, ErrNotFound) {
			t.Fatalf("unreadable file must be reported, not treated as missing: %v", err)
		}
	}
}

func TestCheckHelpers(t *testing.T) {
	for _, u := range []string{"http://localhost:8123", "https://ch.example.com:8443/prefix"} {
		if err := CheckURL(u); err != nil {
			t.Errorf("%s: %v", u, err)
		}
	}
	for _, u := range []string{"localhost:8123", "https://", "https://a:b@h", "https://h/?x=1", "tcp://h:9000", "://"} {
		if err := CheckURL(u); err == nil {
			t.Errorf("%s should be rejected", u)
		}
	}
	if !InsecureRemote("http://10.0.0.1:8123") || InsecureRemote("http://127.0.0.1:8123") || InsecureRemote("https://h") {
		t.Error("InsecureRemote")
	}
	for _, n := range []string{"vario", "_x1", "Vario_2"} {
		if CheckDatabaseName(n) != nil {
			t.Errorf("%s should be valid", n)
		}
	}
	for _, n := range []string{"", "1abc", "a-b", "a.b", "a`b", "x; DROP"} {
		if CheckDatabaseName(n) == nil {
			t.Errorf("%q should be rejected", n)
		}
	}
}
