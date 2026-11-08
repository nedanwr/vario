package config

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// PasswordKey is the pseudo-key for the ClickHouse password. It is not stored
// in the configuration file and is never displayed.
const PasswordKey = "clickhouse.password"

// Key describes one setting that `vario config get/set/show` can access.
type Key struct {
	Name string
	Help string
	// ReadOnly keys can be shown but must be changed with `vario config edit`.
	ReadOnly bool

	get func(*Config) any
	set func(*Config, string) error
}

// Value formats the current value for display (strings unquoted).
func (k Key) Value(c *Config) string {
	switch v := k.get(c).(type) {
	case string:
		return v
	case []string:
		return k.Literal(c)
	default:
		return fmt.Sprint(v)
	}
}

// Literal formats the current value as a TOML literal.
func (k Key) Literal(c *Config) string {
	switch v := k.get(c).(type) {
	case string:
		return tomlString(v)
	case Duration:
		return tomlString(v.String())
	case []string:
		parts := make([]string, len(v))
		for i, s := range v {
			parts[i] = tomlString(s)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	default:
		return fmt.Sprint(v)
	}
}

// Set parses raw and stores it in c. It checks the value's type; full
// validation is done by Config.Validate.
func (k Key) Set(c *Config, raw string) error {
	if k.ReadOnly || k.set == nil {
		return fmt.Errorf("%s cannot be set from the command line; use `vario config edit`", k.Name)
	}
	return k.set(c, strings.TrimSpace(raw))
}

// Default formats the key's default value for display.
func (k Key) Default() string { return k.Value(Default()) }

// Change returns the update that stores c's value of k.
func (k Key) Change(c *Config) Change { return Change{Key: k.Name, Literal: k.Literal(c)} }

func strKey(name, help string, f func(*Config) *string) Key {
	return Key{Name: name, Help: help,
		get: func(c *Config) any { return *f(c) },
		set: func(c *Config, s string) error { *f(c) = s; return nil }}
}

func pathKey(name, help string, f func(*Config) *string) Key {
	k := strKey(name, help, f)
	k.set = func(c *Config, s string) error {
		p, err := ExpandPath(s)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		*f(c) = p
		return nil
	}
	return k
}

func boolKey(name, help string, f func(*Config) *bool) Key {
	return Key{Name: name, Help: help,
		get: func(c *Config) any { return *f(c) },
		set: func(c *Config, s string) error {
			v, err := parseBool(s)
			if err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
			*f(c) = v
			return nil
		}}
}

func parseBool(s string) (bool, error) {
	switch strings.ToLower(s) {
	case "true", "yes", "on", "1":
		return true, nil
	case "false", "no", "off", "0":
		return false, nil
	}
	return false, fmt.Errorf("expected true or false, got %q", s)
}

func intKey(name, help string, f func(*Config) *int) Key {
	return Key{Name: name, Help: help,
		get: func(c *Config) any { return *f(c) },
		set: func(c *Config, s string) error {
			v, err := strconv.Atoi(s)
			if err != nil {
				return fmt.Errorf("%s: expected an integer, got %q", name, s)
			}
			*f(c) = v
			return nil
		}}
}

func durKey(name, help string, f func(*Config) *Duration) Key {
	return Key{Name: name, Help: help,
		get: func(c *Config) any { return *f(c) },
		set: func(c *Config, s string) error {
			v, err := time.ParseDuration(s)
			if err != nil {
				return fmt.Errorf("%s: expected a duration such as 30s or 5m, got %q", name, s)
			}
			f(c).Duration = v
			return nil
		}}
}

func sourceKeys(kind string) []Key {
	get := func(c *Config) SourceConfig {
		s, _ := c.Source(kind)
		s.Kind = kind
		return s
	}
	return []Key{
		{Name: "sources." + kind + ".enabled", Help: "collect from this source",
			get: func(c *Config) any { s, ok := c.Source(kind); return ok && s.Enabled },
			set: func(c *Config, raw string) error {
				v, err := parseBool(raw)
				if err != nil {
					return fmt.Errorf("sources.%s.enabled: %w", kind, err)
				}
				s := get(c)
				s.Enabled = v
				c.UpsertSource(s)
				return nil
			}},
		{Name: "sources." + kind + ".path", Help: "source database path (empty: default location)",
			get: func(c *Config) any { return get(c).Path },
			set: func(c *Config, raw string) error {
				p, err := ExpandPath(raw)
				if err != nil {
					return err
				}
				s := get(c)
				s.Path = p
				c.UpsertSource(s)
				return nil
			}},
	}
}

var keys = append([]Key{
	durKey("collector.poll_interval", "how often sources are checked for new events", func(c *Config) *Duration { return &c.Collector.PollInterval }),
	durKey("collector.reconcile_interval", "full re-read of source state (0s disables)", func(c *Config) *Duration { return &c.Collector.ReconcileInterval }),
	intKey("collector.batch_size", "source records read per batch", func(c *Config) *int { return &c.Collector.BatchSize }),
	pathKey("collector.data_dir", "spool, cursors and lease", func(c *Config) *string { return &c.Collector.DataDir }),
	strKey("collector.log_level", "debug, info, warn or error", func(c *Config) *string { return &c.Collector.LogLevel }),

	strKey("privacy.mode", "metrics, messages or full", func(c *Config) *string { return &c.Privacy.Mode }),
	boolKey("privacy.allow_sensitive_content", "required for the messages and full modes", func(c *Config) *bool { return &c.Privacy.AllowSensitiveContent }),
	boolKey("privacy.redact_secrets", "redact known credential formats", func(c *Config) *bool { return &c.Privacy.RedactSecrets }),
	{Name: "privacy.extra_redaction_patterns", Help: "additional RE2 patterns to redact", ReadOnly: true,
		get: func(c *Config) any { return c.Privacy.ExtraPatterns }},
	intKey("privacy.max_field_bytes", "longest text kept per field", func(c *Config) *int { return &c.Privacy.MaxFieldBytes }),

	strKey("clickhouse.url", "ClickHouse HTTP(S) endpoint", func(c *Config) *string { return &c.ClickHouse.URL }),
	strKey("clickhouse.database", "target database", func(c *Config) *string { return &c.ClickHouse.Database }),
	boolKey("clickhouse.create_database", "allow Vario to create the database if missing", func(c *Config) *bool { return &c.ClickHouse.CreateDatabase }),
	strKey("clickhouse.username", "ClickHouse user", func(c *Config) *string { return &c.ClickHouse.Username }),
	pathKey("clickhouse.password_file", "password file (empty: clickhouse-password next to the config)", func(c *Config) *string { return &c.ClickHouse.PasswordFile }),
	durKey("clickhouse.timeout", "request timeout", func(c *Config) *Duration { return &c.ClickHouse.Timeout }),
	durKey("clickhouse.flush_interval", "minimum interval between delivery rounds", func(c *Config) *Duration { return &c.ClickHouse.FlushInterval }),
	intKey("clickhouse.max_insert_rows", "rows per insert", func(c *Config) *int { return &c.ClickHouse.MaxInsertRows }),
	durKey("clickhouse.retry_initial_backoff", "first retry delay", func(c *Config) *Duration { return &c.ClickHouse.RetryInitialBackoff }),
	durKey("clickhouse.retry_max_backoff", "longest retry delay", func(c *Config) *Duration { return &c.ClickHouse.RetryMaxBackoff }),
	pathKey("clickhouse.ca_file", "custom CA bundle (PEM)", func(c *Config) *string { return &c.ClickHouse.CAFile }),
	boolKey("clickhouse.tls_insecure_skip_verify", "disable TLS certificate verification", func(c *Config) *bool { return &c.ClickHouse.TLSInsecureSkipVerify }),
	boolKey("clickhouse.allow_insecure_http", "allow plain http:// to remote hosts", func(c *Config) *bool { return &c.ClickHouse.AllowInsecureHTTP }),

	intKey("spool.max_size_mb", "upper bound for undelivered data on disk", func(c *Config) *int { return &c.Spool.MaxSizeMB }),
}, sourceKeys(SourceT3Code)...)

// Keys lists every supported key in file order.
func Keys() []Key { return keys }

// LookupKey finds a key by name.
func LookupKey(name string) (Key, bool) {
	for _, k := range keys {
		if k.Name == name {
			return k, true
		}
	}
	return Key{}, false
}

// KeyNames lists the supported key names, sorted.
func KeyNames() []string {
	out := make([]string, len(keys))
	for i, k := range keys {
		out[i] = k.Name
	}
	sort.Strings(out)
	return out
}

// Diff returns the changes that turn from into to, for every key whose value differs.
func Diff(from, to *Config) []Change {
	var out []Change
	for _, k := range keys {
		if k.Literal(from) != k.Literal(to) {
			out = append(out, k.Change(to))
		}
	}
	return out
}

// Clone returns a deep copy of c.
func (c *Config) Clone() *Config {
	n := *c
	n.Privacy.ExtraPatterns = append([]string(nil), c.Privacy.ExtraPatterns...)
	n.Sources = append([]SourceConfig(nil), c.Sources...)
	n.defined = make(map[string]bool, len(c.defined))
	for k, v := range c.defined {
		n.defined[k] = v
	}
	return &n
}
