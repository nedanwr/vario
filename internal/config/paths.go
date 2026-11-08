package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// EnvConfig overrides the default configuration file location.
const EnvConfig = "VARIO_CONFIG"

// EnvPassword supplies the ClickHouse password and takes precedence over the password file.
const EnvPassword = "VARIO_CLICKHOUSE_PASSWORD"

// DefaultConfigPath resolves the configuration file path from the environment or platform defaults.
func DefaultConfigPath() (string, error) {
	if p := os.Getenv(EnvConfig); p != "" {
		return ExpandPath(p)
	}
	dir, err := configDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "config.toml"), nil
}

func configDir() (string, error) {
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" && runtime.GOOS != "windows" {
		return filepath.Join(x, "vario"), nil
	}
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "vario"), nil
}

// DefaultDataDir is where the spool, cursors and logs live.
func DefaultDataDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(home, "Library", "Application Support", "vario"), nil
	case "windows":
		if d := os.Getenv("LOCALAPPDATA"); d != "" {
			return filepath.Join(d, "vario"), nil
		}
		return filepath.Join(home, "AppData", "Local", "vario"), nil
	default:
		if x := os.Getenv("XDG_STATE_HOME"); x != "" {
			return filepath.Join(x, "vario"), nil
		}
		return filepath.Join(home, ".local", "state", "vario"), nil
	}
}

// ExpandPath expands a leading ~ and returns an absolute, cleaned path.
func ExpandPath(p string) (string, error) {
	if p == "" {
		return "", nil
	}
	if p == "~" || strings.HasPrefix(p, "~/") || strings.HasPrefix(p, `~\`) {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		p = filepath.Join(home, p[1:])
	}
	return filepath.Abs(p)
}
