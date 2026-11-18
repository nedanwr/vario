package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/nedanwr/vario/internal/config"
	"github.com/nedanwr/vario/internal/privacy"
	"github.com/nedanwr/vario/internal/sources"
	"github.com/nedanwr/vario/internal/sources/claudecode"
	"github.com/nedanwr/vario/internal/sources/t3code"
)

// sourceKind describes how the CLI discovers, identifies and opens one kind of source.
type sourceKind struct {
	kind string
	name string
	// pathHelp describes what the path of the source points at.
	pathHelp string
	defaults func() []string
	detect   func(ctx context.Context, paths []string) []sources.Detection
	id       func(path string) string
	open     func(path string, p *privacy.Policy, batchSize int) sources.Source
	// summary describes a stored cursor for status output.
	summary func(cursor string) string
	// firstExisting makes the default path the first default that exists
	// (sources whose defaults are alternatives, not a single location).
	firstExisting bool
}

var sourceKinds = []sourceKind{
	{
		kind: config.SourceT3Code, name: "T3 Code", pathHelp: "T3 Code's statev2.sqlite",
		defaults: t3code.DefaultPaths, detect: t3code.Detect, id: t3code.SourceID,
		open:    func(path string, p *privacy.Policy, n int) sources.Source { return t3code.New(path, p, n) },
		summary: t3Summary,
	},
	{
		kind: config.SourceClaudeCode, name: "Claude Code", pathHelp: "Claude Code's configuration directory (~/.claude) or its projects directory",
		defaults: claudecode.DefaultPaths, detect: claudecode.Detect, id: claudecode.SourceID,
		open:    func(path string, p *privacy.Policy, n int) sources.Source { return claudecode.New(path, p, n) },
		summary: claudecode.Summary, firstExisting: true,
	},
}

func lookupKind(kind string) (sourceKind, bool) {
	for _, k := range sourceKinds {
		if k.kind == kind {
			return k, true
		}
	}
	return sourceKind{}, false
}

func supportedList() string { return strings.Join(config.SourceKinds, ", ") }

// defaultPath returns the location a source of this kind uses when none is configured.
func (k sourceKind) defaultPath() string {
	d := k.defaults()
	if len(d) == 0 {
		return ""
	}
	if k.firstExisting {
		for _, p := range d {
			if st, err := os.Stat(p); err == nil && st.IsDir() {
				return p
			}
		}
	}
	return d[0]
}

func t3Summary(c string) string {
	var v struct {
		Epoch int64 `json:"epoch"`
		Seq   int64 `json:"seq"`
	}
	if json.Unmarshal([]byte(c), &v) != nil {
		return "-"
	}
	out := fmt.Sprintf("seq %d", v.Seq)
	if v.Epoch > 1 {
		out += fmt.Sprintf(" (epoch %d)", v.Epoch)
	}
	return out
}

// sourceID returns the source identifier of a configured source.
func sourceID(sc config.SourceConfig) string {
	k, ok := lookupKind(sc.Kind)
	if !ok {
		return ""
	}
	return k.id(sourcePath(sc))
}
