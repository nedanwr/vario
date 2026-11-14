package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// EnvConfig overrides the configuration file location.
const EnvConfig = "VARIO_CONFIG"

// EnvHome overrides the Vario home directory (default ~/.vario), which holds the
// default configuration file, password file and local state. Useful for
// isolated installations and tests.
const EnvHome = "VARIO_HOME"

// EnvPassword supplies the ClickHouse password and takes precedence over the password file.
const EnvPassword = "VARIO_CLICKHOUSE_PASSWORD"

// StateFileName is the spool database inside the data directory.
const StateFileName = "state.db"

// Origin describes how the configuration path was chosen.
type Origin string

const (
	FromFlag    Origin = "--config flag"
	FromEnv     Origin = "$" + EnvConfig
	FromHome    Origin = "$" + EnvHome
	FromDefault Origin = "default"
)

// Location is a resolved configuration file path.
type Location struct {
	Path   string
	Origin Origin
}

// HomeDir returns the Vario home directory: $VARIO_HOME or ~/.vario.
func HomeDir() (string, error) {
	if h := os.Getenv(EnvHome); h != "" {
		return ExpandPath(h)
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "", fmt.Errorf("cannot determine the home directory (%v); set $%s or use --config", err, EnvHome)
	}
	return filepath.Join(home, ".vario"), nil
}

// StandardConfigPath is where Vario keeps its configuration by default:
// $VARIO_HOME/config.toml or ~/.vario/config.toml.
func StandardConfigPath() (string, error) {
	home, err := HomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "config.toml"), nil
}

// Resolve determines the configuration file. Precedence: the explicit path
// (--config), $VARIO_CONFIG, $VARIO_HOME/config.toml, ~/.vario/config.toml.
func Resolve(explicit string) (Location, error) {
	if explicit != "" {
		p, err := ExpandPath(explicit)
		return Location{p, FromFlag}, err
	}
	if p := os.Getenv(EnvConfig); p != "" {
		p, err := ExpandPath(p)
		return Location{p, FromEnv}, err
	}
	p, err := StandardConfigPath()
	if err != nil {
		return Location{}, err
	}
	if os.Getenv(EnvHome) != "" {
		return Location{p, FromHome}, nil
	}
	return Location{p, FromDefault}, nil
}

// DefaultConfigPath resolves the configuration path without an explicit override.
func DefaultConfigPath() (string, error) {
	loc, err := Resolve("")
	return loc.Path, err
}

// DefaultDataDir is where the spool, cursors and lease live when
// collector.data_dir is not set: $VARIO_HOME/data or ~/.vario/data.
func DefaultDataDir() (string, error) {
	home, err := HomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "data"), nil
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

// DisplayPath shortens a path inside the home directory to ~/... for output.
func DisplayPath(p string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return p
	}
	if p == home {
		return "~"
	}
	if rel, err := filepath.Rel(home, p); err == nil && !strings.HasPrefix(rel, "..") && !filepath.IsAbs(rel) {
		return "~" + string(filepath.Separator) + rel
	}
	return p
}
