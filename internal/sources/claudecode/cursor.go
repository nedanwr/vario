package claudecode

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"time"
)

const cursorVersion = 1

// cursor is the persisted collection progress of a source.
type cursor struct {
	V int `json:"v"`
	// Clock is the last version assigned to a record (see clock).
	Clock uint64 `json:"clock"`
	// Files maps each transcript's slash-separated path below the root to its state.
	Files map[string]*fileState `json:"files"`
}

func parseCursor(s string) (cursor, bool) {
	var c cursor
	if s == "" || json.Unmarshal([]byte(s), &c) != nil || c.V != cursorVersion {
		return cursor{}, false
	}
	if c.Files == nil {
		c.Files = map[string]*fileState{}
	}
	return c, true
}

func (c *cursor) String() string {
	b, _ := json.Marshal(c)
	return string(b)
}

// Summary describes a cursor for status output.
func Summary(s string) string {
	c, ok := parseCursor(s)
	if !ok {
		return "-"
	}
	var bytes int64
	var bad int64
	for _, f := range c.Files {
		bytes += f.Off
		bad += f.Bad
	}
	out := pluralize(len(c.Files), "transcript") + ", " + humanBytes(bytes) + " read"
	if bad > 0 {
		out += ", " + pluralize(int(bad), "unreadable line") + " skipped"
	}
	return out
}

func pluralize(n int, what string) string {
	if n == 1 {
		return "1 " + what
	}
	return itoa(int64(n)) + " " + what + "s"
}

// fileState is the progress through one transcript.
type fileState struct {
	// Off is the number of bytes consumed; it always ends on a line boundary.
	Off int64 `json:"off"`
	// Size and MTime are the file's size and modification time when it was
	// last read to the end; while both are unchanged there is nothing to read.
	Size  int64 `json:"size,omitempty"`
	MTime int64 `json:"mt,omitempty"`
	// Head is a hash of the first HeadLen bytes (the first line, at most
	// headMax bytes) and Tail one of the TailLen bytes before Off. A file
	// whose head or tail no longer match was replaced or rewritten and is
	// read again from the start.
	HeadLen int    `json:"hl,omitempty"`
	Head    string `json:"hh,omitempty"`
	TailLen int    `json:"tl,omitempty"`
	Tail    string `json:"th,omitempty"`
	// Epoch counts how often the file was found replaced.
	Epoch int `json:"ep,omitempty"`
	// Bad counts lines that could not be parsed (or were too long) and were skipped.
	Bad int64 `json:"bad,omitempty"`
	// LastTS is the last record timestamp seen; records without one are
	// placed at it.
	LastTS string `json:"ts,omitempty"`
	// Convs holds the parse state of the conversations in this file.
	Convs map[string]*convState `json:"c,omitempty"`
	// Compact is set when Convs was dropped because the file was idle. If
	// the file grows again, its lines up to Off are read again, emitting
	// nothing, to rebuild the parse state.
	Compact bool `json:"z,omitempty"`
	// SDK is the harness running the file's T3 Code and Agent SDK lines
	// (HarnessT3Code, or ownSelf for none) and Own the owner of the last line read that had
	// an entrypoint (see harness.go). Both are empty until the file is
	// attributed; it is not read before. Own0 is the owner of the file's
	// first lines, from which a compacted file is replayed.
	SDK  string `json:"sdk,omitempty"`
	Own  string `json:"own,omitempty"`
	Own0 string `json:"own0,omitempty"`
}

// idleAfter is how long a transcript must be unmodified before its parse
// state is dropped from the cursor.
const idleAfter = 30 * time.Minute

// compact drops the parse state of a file that was read to the end and has
// not changed for idleAfter.
func (f *fileState) compact(t transcript, now time.Time) bool {
	if f.Compact || len(f.Convs) == 0 || !f.unchanged(t) || now.Sub(time.Unix(0, t.mod)) < idleAfter {
		return false
	}
	f.Convs, f.Compact = nil, true
	return true
}

func (f *fileState) unchanged(t transcript) bool {
	return f.Off > 0 && f.Size == t.size && f.MTime == t.mod
}

func hashBytes(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:8])
}

func (f *fileState) setHead(line []byte, length int64) {
	n := min(length, headMax)
	b := line
	if int64(len(b)) >= n {
		b = b[:n]
	} else {
		// The line's newline (or skipped content) falls inside the head.
		b = append(append([]byte{}, line...), '\n')
		if int64(len(b)) > n {
			b = b[:n]
		}
	}
	f.HeadLen, f.Head = int(n), hashBytes(b)
}

// setTail records the hash of the bytes before the offset.
func (f *fileState) setTail(r io.ReaderAt) error {
	n := min(f.Off, tailMax)
	if n == 0 {
		f.TailLen, f.Tail = 0, ""
		return nil
	}
	buf := make([]byte, n)
	if _, err := r.ReadAt(buf, f.Off-n); err != nil {
		return err
	}
	f.TailLen, f.Tail = int(n), hashBytes(buf)
	return nil
}

// verify reports whether the file still begins with the content read
// before: not truncated below the offset, same first line, same bytes before
// the offset.
func (f *fileState) verify(r *os.File, size int64) (bool, error) {
	if size < f.Off {
		return false, nil
	}
	check := func(off int64, n int, want string) (bool, error) {
		if n == 0 {
			return true, nil
		}
		buf := make([]byte, n)
		if _, err := r.ReadAt(buf, off); err != nil {
			if err == io.EOF {
				return false, nil
			}
			return false, err
		}
		return hashBytes(buf) == want, nil
	}
	if ok, err := check(0, f.HeadLen, f.Head); !ok || err != nil {
		return ok, err
	}
	return check(f.Off-int64(f.TailLen), f.TailLen, f.Tail)
}

// convState is what a conversation's later lines need from its earlier ones.
// Text is only kept here when the privacy policy collects it, and then
// already redacted.
type convState struct {
	Created   string                `json:"cr"`
	Updated   string                `json:"up,omitempty"`
	Project   string                `json:"p,omitempty"`
	Model     string                `json:"m,omitempty"`
	Perm      string                `json:"pm,omitempty"`
	Version   string                `json:"ver,omitempty"`
	Entry     string                `json:"ent,omitempty"`
	UserType  string                `json:"ut,omitempty"`
	Agent     string                `json:"ag,omitempty"`
	Custom    string                `json:"ct,omitempty"`
	AITitle   string                `json:"at,omitempty"`
	Summary   string                `json:"sm,omitempty"`
	Branch    string                `json:"b,omitempty"`
	Sub       *subInfo              `json:"sub,omitempty"`
	Runs      int64                 `json:"nr,omitempty"`
	Turns     int64                 `json:"nt,omitempty"`
	Events    int64                 `json:"ne,omitempty"`
	Run       *runState             `json:"run,omitempty"`
	Responses []*responseState      `json:"rs,omitempty"`
	Tools     map[string]*toolState `json:"tools,omitempty"`
	ToolOrder []string              `json:"to,omitempty"`
}

// subInfo describes a subagent conversation.
type subInfo struct {
	Parent      string `json:"parent"`
	Type        string `json:"type,omitempty"`
	BuiltIn     *bool  `json:"builtin,omitempty"`
	Depth       *int64 `json:"depth,omitempty"`
	ParentTool  string `json:"tool,omitempty"`
	Model       string `json:"model,omitempty"`
	Description string `json:"desc,omitempty"`
}

// runState is the current run: one prompt and the work it caused.
type runState struct {
	ID        string `json:"id"`
	Ord       int64  `json:"ord"`
	UserMsg   string `json:"msg,omitempty"`
	Requested string `json:"req,omitempty"`
	Started   string `json:"st,omitempty"`
	Completed string `json:"done,omitempty"`
	Status    string `json:"status"`
	Model     string `json:"m,omitempty"`
}

// responseState accumulates one API response, which Claude Code writes as
// one line per content block, each repeating the response's usage. Only the
// most recent responses of a conversation are kept: a response's lines are
// written together.
type responseState struct {
	ID      string `json:"id"`
	Ord     int64  `json:"ord"`
	Run     string `json:"run,omitempty"`
	Started string `json:"st"`
	Model   string `json:"m,omitempty"`
	Request string `json:"req,omitempty"`
	Stop    string `json:"stop,omitempty"`
	Usage   usage  `json:"u"`
}

// toolState is a tool call whose result has not been seen yet.
type toolState struct {
	Name      string `json:"name"`
	Type      string `json:"type"`
	Ord       int64  `json:"ord"`
	Run       string `json:"run,omitempty"`
	Turn      string `json:"turn,omitempty"`
	Started   string `json:"st,omitempty"`
	InBytes   uint64 `json:"inb"`
	Input     string `json:"in,omitempty"`
	Title     string `json:"title,omitempty"`
	FileName  string `json:"file,omitempty"`
	Redact    uint32 `json:"red,omitempty"`
	Truncated bool   `json:"trunc,omitempty"`
	Attrs     string `json:"attrs,omitempty"`
}
