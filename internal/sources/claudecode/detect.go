package claudecode

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/nedanwr/vario/internal/sources"
)

// Detect inspects candidate transcript directories without modifying them.
func Detect(ctx context.Context, paths []string) []sources.Detection {
	var out []sources.Detection
	for _, p := range paths {
		out = append(out, detectOne(ctx, p))
	}
	return out
}

func detectOne(_ context.Context, path string) sources.Detection {
	root := ResolveRoot(path)
	d := sources.Detection{Kind: Kind, Path: root}
	st, err := os.Stat(root)
	if err != nil {
		d.Detail = "not found"
		if !errors.Is(err, os.ErrNotExist) {
			d.Detail = err.Error()
		}
		return d
	}
	d.Exists = true
	if !st.IsDir() {
		d.Detail = "not a directory (expected Claude Code's projects directory)"
		return d
	}
	files, err := listTranscripts(root)
	if err != nil {
		d.Detail = "cannot read: " + err.Error()
		return d
	}
	d.Compatible = true
	d.Detail = "compatible"
	projects := map[string]bool{}
	var sessions, subagents int
	var size, newest int64
	var newestPath string
	for _, f := range files {
		projects[strings.SplitN(f.rel, "/", 2)[0]] = true
		if f.main {
			sessions++
		} else {
			subagents++
		}
		size += f.size
		if f.mod > newest {
			newest, newestPath = f.mod, f.path
		}
	}
	if len(files) == 0 {
		d.Detail = "compatible (no sessions yet)"
	}
	d.Facts = append(d.Facts,
		sources.Fact{Name: "projects", Value: strconv.Itoa(len(projects))},
		sources.Fact{Name: "sessions", Value: strconv.Itoa(sessions)},
		sources.Fact{Name: "subagent transcripts", Value: strconv.Itoa(subagents)},
		sources.Fact{Name: "size", Value: humanBytes(size)},
	)
	if newest > 0 {
		d.Facts = append(d.Facts, sources.Fact{Name: "last activity", Value: time.Unix(0, newest).UTC().Format(time.RFC3339)})
		if v := tailVersion(newestPath); v != "" {
			d.Facts = append(d.Facts, sources.Fact{Name: "claude code version", Value: v})
		}
	}
	d.Facts = append(d.Facts, sources.Fact{Name: "source id", Value: SourceID(root)})
	return d
}

// tailVersion returns the Claude Code version recorded in the last lines of a transcript.
func tailVersion(path string) string {
	f, err := openShared(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return ""
	}
	n := min(st.Size(), 256<<10)
	buf := make([]byte, n)
	if _, err := f.ReadAt(buf, st.Size()-n); err != nil && err != io.EOF {
		return ""
	}
	lines := bytes.Split(buf, []byte("\n"))
	for i := len(lines) - 1; i >= 0; i-- {
		var h struct {
			Version lstr `json:"version"`
		}
		if json.Unmarshal(lines[i], &h) == nil && h.Version != "" {
			return string(h.Version)
		}
	}
	return ""
}

// Report is the result of a full read-only check of the transcripts.
type Report struct {
	Root       string
	Files      int
	Lines      int64
	Bad        int64
	BadFiles   []string
	Unreadable []string
	Versions   []string
	// Recognized counts lines of the record types the collector maps.
	Recognized int64
	// T3Sessions and T3Subagents count the session and subagent
	// transcripts with lines run by T3 Code, which are collected from T3
	// Code (see harness.go).
	T3Sessions, T3Subagents int
}

// Inspect reads every transcript once (read-only) and reports unreadable
// files and lines that cannot be parsed. Paths in the report are relative
// to the root; no transcript content is included.
func Inspect(ctx context.Context, path string) (Report, error) {
	root := ResolveRoot(path)
	r := Report{Root: root}
	l, err := scanTranscripts(root)
	if err != nil {
		return r, err
	}
	files := l.files
	// Directories discovery could not list (keys ending in "/") and entries
	// it could not stat are access errors too.
	r.Unreadable = append(r.Unreadable, l.unreadable...)
	versions := map[string]bool{}
	sdk := map[string]string{}
	for _, t := range files {
		if err := ctx.Err(); err != nil {
			return r, err
		}
		r.Files++
		f, err := openShared(t.path)
		if err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				r.Unreadable = append(r.Unreadable, t.rel+": "+err.Error())
			}
			continue
		}
		var parent *fileState
		if p, ok := sdk[parentRel(t)]; ok {
			parent = &fileState{SDK: p}
		}
		attr := &fileState{}
		if hi, err := scanHeadAll(f, headInfo{}, t.mainSession()); err == nil {
			attr.SDK, attr.Own, _ = hi.attribution(parent.sdk(), true)
		}
		sdk[t.rel] = attr.SDK
		t3 := false
		br := bufio.NewReaderSize(f, 256<<10)
		bad := int64(0)
		for {
			line, complete, skipped, err := readLine(br)
			if err != nil && err != io.EOF {
				r.Unreadable = append(r.Unreadable, fmt.Sprintf("%s: %v", t.rel, err))
				break
			}
			if len(line) == 0 && !complete && skipped == 0 {
				break
			}
			line = bytes.TrimSpace(line)
			if skipped > 0 {
				bad++
				continue
			}
			if len(line) == 0 {
				continue
			}
			r.Lines++
			var h struct {
				Type       lstr `json:"type"`
				Version    lstr `json:"version"`
				Entrypoint lstr `json:"entrypoint"`
			}
			if line[0] != '{' || json.Unmarshal(line, &h) != nil {
				if complete {
					bad++
				}
				continue
			}
			if h.Entrypoint != "" {
				attr.Own = lineOwner(string(h.Entrypoint), attr.SDK)
			}
			if attr.Own == HarnessT3Code {
				t3 = true
			}
			switch h.Type {
			case "user", "assistant", "system":
				r.Recognized++
			}
			if h.Version != "" {
				versions[string(h.Version)] = true
			}
		}
		f.Close()
		if t3 && t.main {
			r.T3Sessions++
		} else if t3 {
			r.T3Subagents++
		}
		if bad > 0 {
			r.Bad += bad
			r.BadFiles = append(r.BadFiles, t.rel)
		}
	}
	for v := range versions {
		r.Versions = append(r.Versions, v)
	}
	sort.Strings(r.Versions)
	return r, nil
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
