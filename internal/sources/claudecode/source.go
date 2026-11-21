// Package claudecode collects conversations, tool executions and token usage
// from Claude Code's local session transcripts.
//
// Claude Code appends one JSON object per line to a transcript per session:
//
//	<config dir>/projects/<encoded project path>/<session id>.jsonl
//	<config dir>/projects/<encoded project path>/<session id>/subagents/agent-<id>.jsonl
//
// (earlier versions wrote subagent transcripts as agent-<id>.jsonl next to
// the session, or interleaved them in the session file with isSidechain set).
// The files are only ever opened for reading, without locks.
//
// Collection is incremental and forward-only: the cursor records, per file,
// how many bytes were consumed (always ending on a complete line), a
// fingerprint of the file's first line and of the bytes just before the
// offset (to detect replaced or rewritten files) and the small amount of
// parse state needed to continue mid-file (the open conversation, run,
// provider response and unanswered tool calls). New lines are therefore read
// exactly once in normal operation; whole files are re-read only by a full
// re-read (after a privacy change, a forced backfill or a replaced file).
package claudecode

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nedanwr/vario/internal/model"
	"github.com/nedanwr/vario/internal/privacy"
	"github.com/nedanwr/vario/internal/sources"
)

const Kind = "claudecode"

// EnvConfigDir is the variable Claude Code reads its configuration
// directory from (default ~/.claude).
const EnvConfigDir = "CLAUDE_CONFIG_DIR"

// DefaultPaths are the transcript directories checked during discovery, in order.
func DefaultPaths() []string {
	var out []string
	if d := os.Getenv(EnvConfigDir); d != "" {
		for _, p := range filepath.SplitList(d) {
			if p != "" {
				out = append(out, filepath.Join(p, "projects"))
			}
		}
	}
	home, err := os.UserHomeDir()
	if err == nil {
		out = append(out, filepath.Join(home, ".claude", "projects"))
		if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
			out = append(out, filepath.Join(x, "claude", "projects"))
		} else {
			out = append(out, filepath.Join(home, ".config", "claude", "projects"))
		}
	}
	seen := map[string]bool{}
	var uniq []string
	for _, p := range out {
		if !seen[p] {
			seen[p] = true
			uniq = append(uniq, p)
		}
	}
	return uniq
}

// ResolveRoot accepts either Claude Code's configuration directory (which
// contains projects/) or the projects directory itself.
func ResolveRoot(path string) string {
	if path == "" {
		return path
	}
	if filepath.Base(path) != "projects" {
		if st, err := os.Stat(filepath.Join(path, "projects")); err == nil && st.IsDir() {
			return filepath.Join(path, "projects")
		}
	}
	return path
}

type Source struct {
	root      string
	id        string
	host      string
	policy    *privacy.Policy
	batchSize int

	mu sync.Mutex // serializes reads; the collector runs one loop per source
}

// New prepares a source for the transcripts under path (Claude Code's
// configuration directory or its projects directory).
func New(path string, policy *privacy.Policy, batchSize int) *Source {
	if batchSize <= 0 {
		batchSize = 500
	}
	root := ResolveRoot(path)
	host, _ := os.Hostname()
	return &Source{root: root, id: SourceID(path), host: host, policy: policy, batchSize: batchSize}
}

// SourceID derives a stable identifier for the transcripts of one Claude
// Code installation: a hash of this machine's identifier (or host name) and
// the transcript directory. It reveals neither.
func SourceID(path string) string {
	sum := sha256.Sum256([]byte(machineID() + "\x00" + filepath.Clean(ResolveRoot(path))))
	return Kind + ":" + hex.EncodeToString(sum[:8])
}

func (s *Source) Kind() string     { return Kind }
func (s *Source) ID() string       { return s.id }
func (s *Source) Location() string { return s.root }
func (s *Source) Close() error     { return nil }

// ErrUnavailable means the transcript directory cannot currently be read.
var ErrUnavailable = errors.New("claude code transcripts unavailable")

// clock returns the version for records produced now: wall-clock
// milliseconds, never lower than (and always above) the last one used, so
// later revisions of a record always win even if the clock steps back.
func clock(last uint64) uint64 {
	v := uint64(time.Now().UnixMilli())
	if v <= last {
		v = last + 1
	}
	return v
}

func (s *Source) sourceRecord(version uint64) model.Record {
	return model.Record{Table: model.TableSources, Key: s.id, Row: &model.SourceRow{
		Envelope:       model.Envelope{SourceID: s.id, Version: version},
		SourceKind:     Kind,
		HostName:       s.host,
		Location:       s.root,
		CollectionMode: s.policy.Mode(),
	}}
}

// transcript is one discovered JSONL file.
type transcript struct {
	rel  string // slash-separated path below the root: the cursor key
	path string
	size int64
	mod  int64
	// session is the session id implied by the location ("" if unknown) and
	// agent the subagent id implied by the file name.
	session string
	agent   string
	// main is set for <session>.jsonl files, whose records belong to that session.
	main bool
}

// list discovers transcripts below the root in a stable order.
func (s *Source) list() ([]transcript, error) {
	st, err := os.Stat(s.root)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	if !st.IsDir() {
		return nil, fmt.Errorf("%w: %s is not a directory", ErrUnavailable, s.root)
	}
	return listTranscripts(s.root)
}

func listTranscripts(root string) ([]transcript, error) {
	projects, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	var out []transcript
	add := func(dir, rel string, e fs.DirEntry, session, agent string, main bool) {
		info, err := e.Info()
		if err != nil || !info.Mode().IsRegular() {
			return
		}
		out = append(out, transcript{rel: rel, path: filepath.Join(dir, e.Name()), size: info.Size(), mod: info.ModTime().UnixNano(),
			session: session, agent: agent, main: main})
	}
	for _, p := range projects {
		if !p.IsDir() {
			continue
		}
		pdir := filepath.Join(root, p.Name())
		entries, err := os.ReadDir(pdir)
		if err != nil {
			continue // unreadable project directories are reported by doctor
		}
		for _, e := range entries {
			name := e.Name()
			switch {
			case !e.IsDir() && strings.HasSuffix(name, ".jsonl"):
				stem := strings.TrimSuffix(name, ".jsonl")
				if a, ok := strings.CutPrefix(stem, "agent-"); ok {
					add(pdir, p.Name()+"/"+name, e, "", a, false)
				} else {
					add(pdir, p.Name()+"/"+name, e, stem, "", true)
				}
			case e.IsDir():
				sdir := filepath.Join(pdir, name, "subagents")
				subs, err := os.ReadDir(sdir)
				if err != nil {
					continue
				}
				for _, se := range subs {
					sn := se.Name()
					if se.IsDir() || !strings.HasSuffix(sn, ".jsonl") {
						continue
					}
					agent := strings.TrimPrefix(strings.TrimSuffix(sn, ".jsonl"), "agent-")
					add(sdir, p.Name()+"/"+name+"/subagents/"+sn, se, name, agent, false)
				}
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].rel < out[j].rel })
	return out, nil
}

// Snapshot reads every transcript. Without a previous cursor, or with
// includeHistory, every file is read from the start and every line is also
// emitted as a raw event. Otherwise only files that changed since prevCursor
// are read, from where it left off (re-reading replaced files in full).
func (s *Source) Snapshot(ctx context.Context, prevCursor string, includeHistory bool, emit func([]model.Record) error) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	files, err := s.list()
	if err != nil {
		return "", err
	}
	prev, ok := parseCursor(prevCursor)
	full := includeHistory || !ok
	cur := &cursor{V: cursorVersion, Files: map[string]*fileState{}}
	if !full {
		cur.Files = prev.Files
	}
	b := &batcher{limit: s.batchSize, emit: emit, version: clock(prev.Clock)}
	if err := emit([]model.Record{s.sourceRecord(b.version)}); err != nil {
		return "", err
	}
	seen := map[string]bool{}
	for _, t := range files {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		seen[t.rel] = true
		fst := cur.Files[t.rel]
		if fst == nil {
			fst = &fileState{}
			cur.Files[t.rel] = fst
		}
		if !full && fst.unchanged(t) {
			fst.compact(t, time.Now())
			continue
		}
		if err := s.readFile(ctx, t, fst, cur.Files[parentRel(t)], b, false); err != nil {
			return "", err
		}
	}
	for rel := range cur.Files {
		if !seen[rel] {
			delete(cur.Files, rel)
		}
	}
	if err := b.flush(); err != nil {
		return "", err
	}
	cur.Clock = b.version
	return cur.String(), nil
}

// Changes reads lines appended since cursor. Files that were replaced or
// truncated are re-read from the start.
func (s *Source) Changes(ctx context.Context, cursorStr string, limit int) (sources.ChangeSet, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := parseCursor(cursorStr)
	if !ok {
		return sources.ChangeSet{Reconcile: "no valid cursor"}, nil
	}
	files, err := s.list()
	if err != nil {
		return sources.ChangeSet{}, err
	}
	if limit <= 0 {
		limit = s.batchSize
	}
	var recs []model.Record
	b := &batcher{limit: limit, version: clock(cur.Clock), emit: func(r []model.Record) error { recs = append(recs, r...); return nil }}
	changed := false
	seen := map[string]bool{}
	more := false
	now := time.Now()
	for _, t := range files {
		seen[t.rel] = true
		fst := cur.Files[t.rel]
		if fst == nil {
			fst = &fileState{}
			cur.Files[t.rel] = fst
			changed = true
		}
		if fst.unchanged(t) {
			if fst.compact(t, now) {
				changed = true
			}
			continue
		}
		if b.full() {
			more = true
			break
		}
		before := *fst
		if err := s.readFile(ctx, t, fst, cur.Files[parentRel(t)], b, true); err != nil {
			return sources.ChangeSet{}, err
		}
		if fst.Off != before.Off || fst.Size != before.Size || fst.MTime != before.MTime || fst.Epoch != before.Epoch {
			changed = true
		}
		if b.full() && fst.Off < t.size {
			more = true
			break
		}
	}
	if !more {
		for rel := range cur.Files {
			if !seen[rel] {
				delete(cur.Files, rel)
				changed = true
			}
		}
	}
	if err := b.flush(); err != nil {
		return sources.ChangeSet{}, err
	}
	if len(recs) == 0 && !changed {
		return sources.ChangeSet{Cursor: cursorStr}, nil
	}
	if len(recs) > 0 {
		cur.Clock = b.version
	}
	return sources.ChangeSet{Records: recs, Cursor: cur.String(), More: more}, nil
}

// batcher collects the records of a read, keeping only the last revision of
// each entity, and hands them on in batches. Raw events are always kept.
//
// Every batch gets its own version, higher than the previous batch's, so that
// an entity revised by a later line of the same read (a tool call whose
// result follows, a response written in several lines) always replaces the
// revision emitted in an earlier batch.
type batcher struct {
	limit   int
	emit    func([]model.Record) error
	version uint64
	recs    []model.Record
	index   map[string]int
	total   int
}

func (b *batcher) add(r model.Record) {
	if b.index == nil {
		b.index = map[string]int{}
	}
	if r.Table != model.TableRawEvents {
		k := r.Table + "/" + r.Key
		if i, ok := b.index[k]; ok {
			b.recs[i] = r
			return
		}
		b.index[k] = len(b.recs)
	}
	b.recs = append(b.recs, r)
	b.total++
}

// full reports whether the read reached its record limit.
func (b *batcher) full() bool { return b.limit > 0 && b.total >= b.limit }

// ready reports whether a complete batch is pending.
func (b *batcher) ready() bool { return len(b.recs) >= max(b.limit, 1) }

func (b *batcher) flush() error {
	if len(b.recs) == 0 {
		return nil
	}
	recs := b.recs
	b.recs, b.index = nil, nil
	b.version++
	return b.emit(recs)
}

const (
	headMax = 4096
	tailMax = 4096
	// maxLine bounds the memory used for one line; longer lines are skipped.
	maxLine = 64 << 20
)

// readFile reads t from fst's offset (or from the start if the file was
// replaced) and feeds every complete line to the normalizer. With bounded set
// it stops after the line at which the batcher reached its limit (in
// Changes, whose records are returned as one batch); otherwise it hands on
// complete batches as it goes.
func (s *Source) readFile(ctx context.Context, t transcript, fst *fileState, parent *fileState, b *batcher, bounded bool) error {
	f, err := openShared(t.path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil // deleted while listing; dropped on the next read
		}
		return fmt.Errorf("open %s: %w", t.rel, err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return fmt.Errorf("stat %s: %w", t.rel, err)
	}
	size := st.Size()
	if fst.Off > 0 {
		same, err := fst.verify(f, size)
		if err != nil {
			return fmt.Errorf("read %s: %w", t.rel, err)
		}
		if !same {
			*fst = fileState{Epoch: fst.Epoch + 1}
		}
	}
	if fst.Own == "" {
		ok, err := attribute(f, t, fst, parent, st.ModTime())
		if err != nil {
			return fmt.Errorf("read %s: %w", t.rel, err)
		}
		if !ok {
			return nil // read once the lines that decide it are written
		}
	}
	n := newNormalizer(s, t, fst, b)
	start := fst.Off
	// A compacted file is read from the start to rebuild its parse state;
	// lines before the offset were collected already and emit nothing.
	replayTo, bad := int64(0), fst.Bad
	if fst.Compact {
		replayTo, start, fst.Compact = fst.Off, 0, false
		n.quiet = true
		if fst.Own0 != "" {
			fst.Own = fst.Own0
		}
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return fmt.Errorf("seek %s: %w", t.rel, err)
	}
	r := bufio.NewReaderSize(io.LimitReader(f, size-start), 256<<10)
	off := start
	eof := false
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		line, complete, skipped, err := readLine(r)
		if len(line) == 0 && !complete && skipped == 0 {
			if err != nil && err != io.EOF {
				return fmt.Errorf("read %s: %w", t.rel, err)
			}
			eof = true
			break
		}
		if !complete {
			// A trailing line without its newline is consumed only if it is
			// already a whole JSON value (Claude Code writes a line and its
			// newline together, so a valid prefix cannot be a partial line).
			trimmed := bytes.TrimSpace(line)
			if len(trimmed) == 0 || !json.Valid(trimmed) {
				eof = true
				break
			}
		}
		length := int64(len(line)) + skipped
		if complete {
			length++ // newline
		}
		if off == 0 && !n.quiet {
			head := line
			if skipped > 0 {
				// The line was too long to keep: read its prefix again.
				head = make([]byte, min(length, headMax))
				if _, err := f.ReadAt(head, 0); err != nil {
					return fmt.Errorf("read %s: %w", t.rel, err)
				}
			}
			fst.setHead(head, length)
		}
		if skipped > 0 {
			fst.Bad++
		} else {
			n.line(off, line)
		}
		off += length
		if n.quiet {
			if off < replayTo {
				continue
			}
			n.quiet, fst.Bad = false, bad
			clear(n.dirty)
		}
		fst.Off = off
		if off >= size {
			eof = true
		}
		if bounded && b.full() {
			break
		}
		if !bounded && b.ready() {
			n.finish()
			if err := b.flush(); err != nil {
				return err
			}
		}
	}
	if n.quiet {
		// The file ended before the replay did (it is checked against the
		// offset first, so only if it changed meanwhile): keep it compacted.
		fst.Convs, fst.Compact, fst.Bad = nil, true, bad
		return nil
	}
	n.finish()
	if eof {
		// Record what was seen only once everything up to it was read, so
		// that an unchanged size and modification time later mean that
		// nothing is left to read.
		fst.Size, fst.MTime = size, st.ModTime().UnixNano()
	} else {
		fst.Size, fst.MTime = 0, 0
	}
	return fst.setTail(f)
}

// readLine returns the next line without its newline. complete is false at
// the end of input without a newline. Lines longer than maxLine are skipped:
// their length is returned in skipped and line is empty.
func readLine(r *bufio.Reader) (line []byte, complete bool, skipped int64, err error) {
	for {
		chunk, e := r.ReadSlice('\n')
		switch {
		case skipped > 0:
			skipped += int64(len(chunk))
		case len(line)+len(chunk) > maxLine:
			skipped = int64(len(line) + len(chunk))
			line = nil
		default:
			line = append(line, chunk...)
		}
		if e == nil {
			if skipped > 0 {
				return nil, true, skipped - 1, nil
			}
			return line[:len(line)-1], true, 0, nil
		}
		if e != bufio.ErrBufferFull {
			return line, false, skipped, e
		}
	}
}
