// Package buildinfo exposes version metadata injected at build time.
package buildinfo

import (
	"fmt"
	"runtime"
	"runtime/debug"
)

// Set with -ldflags "-X github.com/nedanwr/vario/internal/buildinfo.Version=v0.1.0 ...".
var (
	Version = "dev"
	Commit  = ""
	Date    = ""
)

type Info struct {
	Version   string
	Commit    string
	Date      string
	GoVersion string
	Platform  string
	Modified  bool
}

func Get() Info {
	info := Info{
		Version:   Version,
		Commit:    Commit,
		Date:      Date,
		GoVersion: runtime.Version(),
		Platform:  runtime.GOOS + "/" + runtime.GOARCH,
	}
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, s := range bi.Settings {
			switch s.Key {
			case "vcs.revision":
				if info.Commit == "" {
					info.Commit = s.Value
				}
			case "vcs.time":
				if info.Date == "" {
					info.Date = s.Value
				}
			case "vcs.modified":
				info.Modified = s.Value == "true"
			}
		}
		if info.Version == "dev" && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
			info.Version = bi.Main.Version
		}
	}
	return info
}

func (i Info) String() string {
	commit := i.Commit
	if len(commit) > 12 {
		commit = commit[:12]
	}
	if commit == "" {
		commit = "unknown"
	}
	if i.Modified {
		commit += "-dirty"
	}
	date := i.Date
	if date == "" {
		date = "unknown"
	}
	return fmt.Sprintf("vario %s (commit %s, built %s, %s, %s)", i.Version, commit, date, i.GoVersion, i.Platform)
}

// UserAgent identifies Vario to user-configured endpoints only.
func UserAgent() string { return "vario/" + Version }
