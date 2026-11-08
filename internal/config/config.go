// Package config loads, validates and writes Vario's configuration file.
package config

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"text/template"
	"time"

	"github.com/BurntSushi/toml"
)

// Collection modes, from least to most sensitive.
const (
	ModeMetrics  = "metrics"
	ModeMessages = "messages"
	ModeFull     = "full"
)

const SourceT3Code = "t3code"

type Duration struct{ time.Duration }

func (d *Duration) UnmarshalText(b []byte) error {
	v, err := time.ParseDuration(string(b))
	if err != nil {
		return err
	}
	d.Duration = v
	return nil
}

func (d Duration) MarshalText() ([]byte, error) { return []byte(d.String()), nil }

type Config struct {
	Collector  CollectorConfig  `toml:"collector"`
	Privacy    PrivacyConfig    `toml:"privacy"`
	ClickHouse ClickHouseConfig `toml:"clickhouse"`
	Spool      SpoolConfig      `toml:"spool"`
	Sources    []SourceConfig   `toml:"sources"`

	path        string
	fingerprint string
	defined     map[string]bool
}

type CollectorConfig struct {
	PollInterval      Duration `toml:"poll_interval"`
	ReconcileInterval Duration `toml:"reconcile_interval"`
	BatchSize         int      `toml:"batch_size"`
	DataDir           string   `toml:"data_dir"`
	LogLevel          string   `toml:"log_level"`
}

type PrivacyConfig struct {
	Mode string `toml:"mode"`
	// AllowSensitiveContent is the explicit opt-in required for the messages and full modes.
	AllowSensitiveContent bool     `toml:"allow_sensitive_content"`
	RedactSecrets         bool     `toml:"redact_secrets"`
	ExtraPatterns         []string `toml:"extra_redaction_patterns"`
	MaxFieldBytes         int      `toml:"max_field_bytes"`
}

type ClickHouseConfig struct {
	URL      string `toml:"url"`
	Database string `toml:"database"`
	// CreateDatabase allows Vario to run CREATE DATABASE when the database is
	// missing. When false (the default) Vario only uses an existing database.
	CreateDatabase        bool     `toml:"create_database"`
	Username              string   `toml:"username"`
	PasswordFile          string   `toml:"password_file"`
	Timeout               Duration `toml:"timeout"`
	FlushInterval         Duration `toml:"flush_interval"`
	MaxInsertRows         int      `toml:"max_insert_rows"`
	RetryInitialBackoff   Duration `toml:"retry_initial_backoff"`
	RetryMaxBackoff       Duration `toml:"retry_max_backoff"`
	CAFile                string   `toml:"ca_file"`
	TLSInsecureSkipVerify bool     `toml:"tls_insecure_skip_verify"`
	AllowInsecureHTTP     bool     `toml:"allow_insecure_http"`
}

type SpoolConfig struct {
	MaxSizeMB int `toml:"max_size_mb"`
}

type SourceConfig struct {
	Kind    string `toml:"kind"`
	Enabled bool   `toml:"enabled"`
	Path    string `toml:"path"`
}

// Default returns a configuration with conservative defaults and no sources.
func Default() *Config {
	return &Config{
		Collector: CollectorConfig{
			PollInterval:      Duration{5 * time.Second},
			ReconcileInterval: Duration{6 * time.Hour},
			BatchSize:         500,
			LogLevel:          "info",
		},
		Privacy: PrivacyConfig{
			Mode:          ModeMetrics,
			RedactSecrets: true,
			MaxFieldBytes: 64 * 1024,
		},
		ClickHouse: ClickHouseConfig{
			Database:            "vario",
			Username:            "default",
			Timeout:             Duration{30 * time.Second},
			FlushInterval:       Duration{10 * time.Second},
			MaxInsertRows:       5000,
			RetryInitialBackoff: Duration{2 * time.Second},
			RetryMaxBackoff:     Duration{5 * time.Minute},
		},
		Spool: SpoolConfig{MaxSizeMB: 512},
	}
}

// ErrNotFound is returned when the configuration file does not exist.
var ErrNotFound = errors.New("configuration file not found")

// Load reads the configuration at path, applying defaults for omitted settings.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w: %s (run `vario init`)", ErrNotFound, path)
	}
	if err != nil {
		return nil, fmt.Errorf("read configuration: %w", err)
	}
	return Parse(path, data)
}

// Parse decodes configuration data as if it had been read from path.
// Unknown settings are rejected so that typos are not silently ignored.
func Parse(path string, data []byte) (*Config, error) {
	cfg := Default()
	cfg.path = path
	cfg.fingerprint = fingerprint(data)
	md, err := toml.Decode(string(data), cfg)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		keys := make([]string, len(undecoded))
		for i, k := range undecoded {
			keys[i] = k.String()
		}
		return nil, fmt.Errorf("parse %s: unknown settings: %s", path, strings.Join(keys, ", "))
	}
	cfg.defined = map[string]bool{}
	for _, k := range md.Keys() {
		cfg.defined[k.String()] = true
	}
	if err := cfg.resolvePaths(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// FingerprintBytes returns the fingerprint of configuration file content (see Config.Fingerprint).
func FingerprintBytes(data []byte) string { return fingerprint(data) }

func fingerprint(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

// Fingerprint identifies the file content the configuration was loaded from
// (empty for configurations that were not loaded from a file).
func (c *Config) Fingerprint() string { return c.fingerprint }

// IsSet reports whether a table-level key such as "clickhouse.url" is set in
// the loaded file (as opposed to taking its default).
func (c *Config) IsSet(key string) bool { return c.defined[key] }

func (c *Config) Path() string { return c.path }

func (c *Config) SetPath(p string) { c.path = p }

func (c *Config) resolvePaths() error {
	var err error
	if c.Collector.DataDir == "" {
		if c.Collector.DataDir, err = DefaultDataDir(); err != nil {
			return err
		}
	} else if c.Collector.DataDir, err = ExpandPath(c.Collector.DataDir); err != nil {
		return err
	}
	if c.ClickHouse.PasswordFile != "" {
		if c.ClickHouse.PasswordFile, err = ExpandPath(c.ClickHouse.PasswordFile); err != nil {
			return err
		}
	}
	if c.ClickHouse.CAFile != "" {
		if c.ClickHouse.CAFile, err = ExpandPath(c.ClickHouse.CAFile); err != nil {
			return err
		}
	}
	for i := range c.Sources {
		if c.Sources[i].Path != "" {
			if c.Sources[i].Path, err = ExpandPath(c.Sources[i].Path); err != nil {
				return err
			}
		}
	}
	return nil
}

var identRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Validate reports every configuration problem found.
func (c *Config) Validate() error {
	var errs []error
	add := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }

	if c.Collector.PollInterval.Duration < 500*time.Millisecond {
		add("collector.poll_interval must be at least 500ms")
	}
	if c.Collector.ReconcileInterval.Duration != 0 && c.Collector.ReconcileInterval.Duration < time.Minute {
		add("collector.reconcile_interval must be 0 (disabled) or at least 1m")
	}
	if c.Collector.BatchSize < 1 || c.Collector.BatchSize > 50000 {
		add("collector.batch_size must be between 1 and 50000")
	}
	switch strings.ToLower(c.Collector.LogLevel) {
	case "debug", "info", "warn", "error":
	default:
		add("collector.log_level must be one of debug, info, warn, error")
	}

	switch c.Privacy.Mode {
	case ModeMetrics:
	case ModeMessages, ModeFull:
		if !c.Privacy.AllowSensitiveContent {
			add("privacy.mode %q collects conversation content and requires privacy.allow_sensitive_content = true", c.Privacy.Mode)
		}
	default:
		add("privacy.mode must be one of %s, %s, %s", ModeMetrics, ModeMessages, ModeFull)
	}
	for _, p := range c.Privacy.ExtraPatterns {
		if _, err := regexp.Compile(p); err != nil {
			add("privacy.extra_redaction_patterns: invalid pattern %q: %v", p, err)
		}
	}
	if c.Privacy.MaxFieldBytes < 0 {
		add("privacy.max_field_bytes must not be negative")
	}

	if err := c.ClickHouse.validate(); err != nil {
		errs = append(errs, err)
	}
	if c.Spool.MaxSizeMB < 16 {
		add("spool.max_size_mb must be at least 16")
	}

	seen := map[string]bool{}
	for i, s := range c.Sources {
		if s.Kind != SourceT3Code {
			add("sources[%d].kind %q is not supported (supported: %s)", i, s.Kind, SourceT3Code)
		}
		if seen[s.Kind] {
			add("sources[%d]: source %q is configured more than once", i, s.Kind)
		}
		seen[s.Kind] = true
	}
	return errors.Join(errs...)
}

func (c ClickHouseConfig) validate() error {
	var errs []error
	add := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }
	if c.URL == "" {
		add("clickhouse.url is required")
	} else if err := CheckURL(c.URL); err != nil {
		add("clickhouse.url %v", err)
	} else if InsecureRemote(c.URL) && !c.AllowInsecureHTTP {
		add("clickhouse.url uses unencrypted http for a remote host; use https or set clickhouse.allow_insecure_http = true")
	}
	if err := CheckDatabaseName(c.Database); err != nil {
		add("clickhouse.database %v", err)
	}
	if c.Username == "" {
		add("clickhouse.username is required")
	}
	if c.Timeout.Duration < time.Second {
		add("clickhouse.timeout must be at least 1s")
	}
	if c.FlushInterval.Duration < 100*time.Millisecond {
		add("clickhouse.flush_interval must be at least 100ms")
	}
	if c.MaxInsertRows < 1 {
		add("clickhouse.max_insert_rows must be positive")
	}
	if c.RetryInitialBackoff.Duration <= 0 || c.RetryMaxBackoff.Duration < c.RetryInitialBackoff.Duration {
		add("clickhouse retry backoff must be positive and retry_max_backoff >= retry_initial_backoff")
	}
	return errors.Join(errs...)
}

// CheckURL validates the syntax of a ClickHouse HTTP(S) endpoint.
func CheckURL(raw string) error {
	u, err := url.Parse(raw)
	switch {
	case err != nil:
		return fmt.Errorf("is not a valid URL: %v", err)
	case u.Scheme != "http" && u.Scheme != "https":
		return errors.New("must use http or https (the ClickHouse HTTP interface, e.g. http://localhost:8123)")
	case u.Host == "" || u.Hostname() == "":
		return errors.New("must include a host, e.g. http://localhost:8123")
	case u.User != nil:
		return errors.New("must not embed credentials; use clickhouse.username and the password file")
	case u.RawQuery != "" || u.Fragment != "":
		return errors.New("must not contain a query string or fragment")
	}
	return nil
}

// InsecureRemote reports whether raw is a plain http:// URL for a non-loopback host.
func InsecureRemote(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "http" && !IsLoopback(u.Hostname())
}

// CheckDatabaseName validates a ClickHouse database name. Only plain
// identifiers are accepted so that names never need unusual quoting.
func CheckDatabaseName(name string) error {
	if !identRe.MatchString(name) || len(name) > 128 {
		return errors.New("must be a plain identifier (letters, digits and underscores, not starting with a digit)")
	}
	return nil
}

// IsLoopback reports whether host refers to the local machine.
func IsLoopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// Source returns the configuration for a source kind.
func (c *Config) Source(kind string) (SourceConfig, bool) {
	for _, s := range c.Sources {
		if s.Kind == kind {
			return s, true
		}
	}
	return SourceConfig{}, false
}

// UpsertSource adds or replaces the configuration for a source kind.
func (c *Config) UpsertSource(s SourceConfig) {
	for i := range c.Sources {
		if c.Sources[i].Kind == s.Kind {
			c.Sources[i] = s
			return
		}
	}
	c.Sources = append(c.Sources, s)
}

// PasswordPath returns the password file location, defaulting to a file next to the config.
func (c *Config) PasswordPath() string {
	if c.ClickHouse.PasswordFile != "" {
		return c.ClickHouse.PasswordFile
	}
	return filepath.Join(filepath.Dir(c.path), "clickhouse-password")
}

// Password reads the ClickHouse password from the environment or the password file.
// A missing password file yields an empty password.
func (c *Config) Password() (string, error) {
	if p, ok := os.LookupEnv(EnvPassword); ok {
		return p, nil
	}
	data, err := os.ReadFile(c.PasswordPath())
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read password file: %w", err)
	}
	return strings.TrimRight(string(data), "\r\n"), nil
}

// WritePassword stores the password in a file readable only by the current user.
func (c *Config) WritePassword(pw string) error {
	return writePrivate(c.PasswordPath(), []byte(pw))
}

// Render returns the configuration as a complete, commented TOML document.
func (c *Config) Render() ([]byte, error) {
	var buf bytes.Buffer
	if err := fileTemplate.Execute(&buf, c); err != nil {
		return nil, err
	}
	var check Config
	if _, err := toml.Decode(buf.String(), &check); err != nil {
		return nil, fmt.Errorf("internal error: generated config does not parse: %w", err)
	}
	return buf.Bytes(), nil
}

// Save writes the configuration file with restrictive permissions,
// replacing any existing file. Prefer Create or Update, which never discard
// an existing file's content unintentionally.
func (c *Config) Save() error {
	if c.path == "" {
		return errors.New("config path not set")
	}
	data, err := c.Render()
	if err != nil {
		return err
	}
	unlock, err := lockFile(c.path)
	if err != nil {
		return err
	}
	defer unlock()
	return writePrivate(c.path, data)
}

// ErrExists is returned by Create when the configuration file already exists.
var ErrExists = errors.New("configuration file already exists")

// Create writes a new configuration file from c. If a file exists, it fails
// with ErrExists unless replace is set, in which case the existing file is
// first copied to a timestamped backup whose path is returned.
func (c *Config) Create(replace bool) (backup string, err error) {
	data, err := c.Render()
	if err != nil {
		return "", err
	}
	return writeNew(c.path, data, replace)
}

// writeNew atomically writes data as a new configuration file at path (see Create).
func writeNew(path string, data []byte, replace bool) (backup string, err error) {
	if path == "" {
		return "", errors.New("config path not set")
	}
	unlock, err := lockFile(path)
	if err != nil {
		return "", err
	}
	defer unlock()
	old, err := os.ReadFile(path)
	switch {
	case err == nil && !replace:
		return "", fmt.Errorf("%w: %s", ErrExists, path)
	case err == nil:
		backup = path + ".bak-" + time.Now().Format("20060102-150405")
		if err := writePrivate(backup, old); err != nil {
			return "", fmt.Errorf("back up existing configuration: %w", err)
		}
	case !errors.Is(err, os.ErrNotExist):
		return "", fmt.Errorf("read existing configuration: %w", err)
	}
	return backup, writePrivate(path, data)
}

// ClearPassword removes the password file. A missing file is not an error.
func (c *Config) ClearPassword() error {
	err := os.Remove(c.PasswordPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// HasStoredPassword reports whether the password file exists.
func (c *Config) HasStoredPassword() bool {
	_, err := os.Stat(c.PasswordPath())
	return err == nil
}

// writePrivate atomically replaces path with data (mode 0600), creating the
// parent directory with mode 0700 if needed. A failure leaves any existing
// file untouched.
func writePrivate(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync() // persist the rename; unsupported on some platforms
		d.Close()
	}
	return nil
}

func tomlString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"':
			b.WriteString(`\"`)
		case r == '\\':
			b.WriteString(`\\`)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, `\u%04X`, r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

var fileTemplate = template.Must(template.New("config").Funcs(template.FuncMap{
	"q": tomlString,
	"list": func(ss []string) string {
		parts := make([]string, len(ss))
		for i, s := range ss {
			parts[i] = tomlString(s)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	},
}).Parse(`# Vario configuration. See README.md for every option.
# This file is readable only by its owner. The ClickHouse password is stored
# separately (clickhouse.password_file) or supplied via VARIO_CLICKHOUSE_PASSWORD.
# Change settings with: vario config set <key> <value>  (or: vario config edit)

[collector]
poll_interval = {{q .Collector.PollInterval.String}}
reconcile_interval = {{q .Collector.ReconcileInterval.String}}
batch_size = {{.Collector.BatchSize}}
data_dir = {{q .Collector.DataDir}}
log_level = {{q .Collector.LogLevel}}

[privacy]
# metrics  - token usage, timings, tool names and statuses; no conversation text.
# messages - adds user/assistant message text, conversation and project titles.
# full     - adds tool inputs/outputs, reasoning, file diffs, web searches and raw event payloads.
mode = {{q .Privacy.Mode}}
# Must be true for the messages and full modes.
allow_sensitive_content = {{.Privacy.AllowSensitiveContent}}
redact_secrets = {{.Privacy.RedactSecrets}}
extra_redaction_patterns = {{list .Privacy.ExtraPatterns}}
max_field_bytes = {{.Privacy.MaxFieldBytes}}

[clickhouse]
url = {{q .ClickHouse.URL}}
database = {{q .ClickHouse.Database}}
# false: use an existing database (Vario never runs CREATE DATABASE).
# true:  create the database if it is missing (requires CREATE DATABASE).
create_database = {{.ClickHouse.CreateDatabase}}
username = {{q .ClickHouse.Username}}
{{- if .ClickHouse.PasswordFile}}
password_file = {{q .ClickHouse.PasswordFile}}
{{- end}}
timeout = {{q .ClickHouse.Timeout.String}}
flush_interval = {{q .ClickHouse.FlushInterval.String}}
max_insert_rows = {{.ClickHouse.MaxInsertRows}}
retry_initial_backoff = {{q .ClickHouse.RetryInitialBackoff.String}}
retry_max_backoff = {{q .ClickHouse.RetryMaxBackoff.String}}
{{- if .ClickHouse.CAFile}}
ca_file = {{q .ClickHouse.CAFile}}
{{- end}}
tls_insecure_skip_verify = {{.ClickHouse.TLSInsecureSkipVerify}}
allow_insecure_http = {{.ClickHouse.AllowInsecureHTTP}}

[spool]
max_size_mb = {{.Spool.MaxSizeMB}}
{{range .Sources}}
[[sources]]
kind = {{q .Kind}}
enabled = {{.Enabled}}
path = {{q .Path}}
{{end}}`))
