package claudecode

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"time"
)

// Sessions run by T3 Code.
//
// T3 Code runs Claude through the Agent SDK, so its Claude threads are also
// ordinary Claude Code transcripts. Vario collects T3 Code from its own
// database, which records each thread's conversation, tool calls and the
// main agent's token usage (exactly the sum of the transcript's main-agent
// responses). It does not record the usage of subagents that Claude starts
// within a thread: it shows them as child threads with their messages and
// tool calls but no token usage.
//
// So that every API call is counted once, and by the source that recorded the
// work, lines run by T3 Code are attributed to T3 Code:
//
//   - main-agent lines are not collected here at all;
//   - subagent lines contribute only their responses' usage (provider turns
//     with usage_scope 'subagent'), not conversations, runs, messages,
//     execution events or raw events, which T3 Code's child thread holds.
//
// This attribution does not depend on whether a T3 Code source is
// configured: neither source can retract rows it has delivered, so switching
// attribution with the configuration would count the same work twice. Doctor
// warns when such sessions exist and T3 Code is not collected.
//
// A line is run by T3 Code if its entrypoint is T3 Code's ("t3-code") or an
// Agent SDK one ("sdk-ts", ...; T3 Code builds before it set its own), and
// its session loaded T3 Code's MCP tools before its first response (a
// subagent: its session did). The entrypoint alone does not decide: T3 Code
// sets it in the environment of the Claude it runs, so any claude that Claude
// starts in turn (`claude -p` from a shell command) inherits it, and T3 Code
// knows nothing of those sessions. Lines without an entrypoint (titles, queue
// operations) belong to whoever wrote the last line that had one. Within a
// session, a later segment from another entrypoint (a T3 Code session resumed
// with `claude --resume`) is attributed to that entrypoint and collected.
const (
	// HarnessT3Code identifies sessions run by T3 Code.
	HarnessT3Code = "t3code"
	// ownSelf marks lines run by Claude Code itself (any other entrypoint).
	ownSelf = "-"

	t3Entrypoint = "t3-code"
	// t3MCPMarker is how T3 Code's MCP tools appear in the attachments that
	// list the tools available to a session.
	t3MCPMarker = `"mcp__t3-code__`

	// headScanMax bounds one step of the scan that attributes a transcript.
	// A longer start is scanned in several steps; a transcript left
	// undecided keeps its progress (see fileState.Scan).
	headScanMax = 8 << 20
	// settleAfter is how long a transcript whose attribution is still open
	// (no entrypoint yet, or a T3 Code or Agent SDK session without a
	// response yet)
	// waits for the lines that decide it before it is attributed with what
	// it has. Until then it is not collected; nothing in it is billable yet.
	settleAfter = 10 * time.Minute
)

// lineOwner attributes a line with the given entrypoint, given the harness
// of the session's Agent SDK lines.
func lineOwner(entry, sdk string) string {
	if harnessEntry(entry) && sdk == HarnessT3Code {
		return HarnessT3Code
	}
	return ownSelf
}

// harnessEntry reports whether entry is one T3 Code runs Claude with.
func harnessEntry(entry string) bool {
	return entry == t3Entrypoint || strings.HasPrefix(entry, "sdk-")
}

// headInfo is what the start of a transcript says about who runs it.
type headInfo struct {
	entry    string // the first entrypoint
	marker   bool   // T3 Code's MCP tools were listed before the first response
	response bool   // a response was reached
	end      bool   // the end of the file was reached
	more     bool   // the scan step ended before the first response or the end
	next     int64  // where the scan continues: the end of the last complete line
}

// scanHead continues hi's scan of a transcript, from hi.next up to its first
// response, the end of the file, or the first line that starts after
// headScanMax more bytes (hi.more).
func scanHead(f io.ReaderAt, hi headInfo) (headInfo, error) {
	hi.end, hi.more = false, false
	if hi.response {
		return hi, nil
	}
	br := bufio.NewReaderSize(io.NewSectionReader(f, hi.next, 1<<62), 64<<10)
	var read int64
	for {
		if read >= headScanMax {
			hi.more = true
			return hi, nil
		}
		line, complete, skipped, err := readLine(br)
		if len(line) == 0 && !complete && skipped == 0 {
			if err != nil && err != io.EOF {
				return hi, err
			}
			hi.end = true
			return hi, nil
		}
		if complete {
			n := int64(len(line)) + skipped + 1
			read += n
			hi.next += n
		} else if !json.Valid(bytes.TrimSpace(line)) {
			// A line still being written.
			hi.end = true
			return hi, nil
		}
		var h struct {
			Type       lstr `json:"type"`
			Entrypoint lstr `json:"entrypoint"`
		}
		if json.Unmarshal(line, &h) != nil {
			continue
		}
		if hi.entry == "" {
			hi.entry = string(h.Entrypoint)
		}
		if h.Type == "attachment" && bytes.Contains(line, []byte(t3MCPMarker)) {
			hi.marker = true
		}
		if h.Type == "assistant" {
			hi.response = true
			return hi, nil
		}
	}
}

// scanHeadAll continues hi's scan step by step up to the first response or
// the end of the file.
func scanHeadAll(f io.ReaderAt, hi headInfo) (headInfo, error) {
	for {
		var err error
		if hi, err = scanHead(f, hi); err != nil || !hi.more {
			return hi, err
		}
	}
}

// headScan is the progress of attributing a transcript whose start is longer
// than one scan step. Tail is a hash of the bytes before Off; a file whose
// bytes there changed was replaced and is scanned again from the start.
type headScan struct {
	Off      int64  `json:"off"`
	Tail     string `json:"th"`
	Entry    string `json:"ent,omitempty"`
	Marker   bool   `json:"mk,omitempty"`
	Response bool   `json:"rsp,omitempty"`
}

// resumeHead returns where fst's scan of f continues.
func (fst *fileState) resumeHead(f io.ReaderAt) (headInfo, error) {
	sc := fst.Scan
	if sc == nil || sc.Off <= 0 {
		return headInfo{}, nil
	}
	buf := make([]byte, min(sc.Off, tailMax))
	if _, err := f.ReadAt(buf, sc.Off-int64(len(buf))); err != nil {
		if err == io.EOF {
			return headInfo{}, nil // truncated: replaced
		}
		return headInfo{}, err
	}
	if hashBytes(buf) != sc.Tail {
		return headInfo{}, nil
	}
	return headInfo{entry: sc.Entry, marker: sc.Marker, response: sc.Response, next: sc.Off}, nil
}

// saveHead records hi's progress in fst.
func (fst *fileState) saveHead(f io.ReaderAt, hi headInfo) error {
	if hi.next <= 0 {
		fst.Scan = nil
		return nil
	}
	buf := make([]byte, min(hi.next, tailMax))
	if _, err := f.ReadAt(buf, hi.next-int64(len(buf))); err != nil {
		return err
	}
	fst.Scan = &headScan{Off: hi.next, Tail: hashBytes(buf), Entry: hi.entry, Marker: hi.marker, Response: hi.response}
	return nil
}

// attribute decides who runs the file f (see harness.go) and records it in
// fst. ok is false while the start of the file does not decide it yet; a
// file that was already partly read (by a version that did not attribute
// files) is decided with what it has.
func attribute(f io.ReaderAt, t transcript, fst, parent *fileState, mod time.Time) (bool, error) {
	hi, err := fst.resumeHead(f)
	if err != nil {
		return false, err
	}
	if hi, err = scanHeadAll(f, hi); err != nil {
		return false, err
	}
	settled := fst.Off > 0 || time.Since(mod) >= settleAfter
	ok := false
	var sdk, own string
	if parentRel(t) == "" || parent.sdk() != "" || settled {
		sdk, own, ok = hi.attribution(parent.sdk(), settled)
	} // else a subagent inherits its session's attribution: wait for it.
	if !ok {
		return false, fst.saveHead(f, hi)
	}
	fst.SDK, fst.Own, fst.Own0, fst.Scan = sdk, own, own, nil
	return true, nil
}

// parentRel returns the cursor key of the session transcript a subagent
// transcript in the current layout belongs to ("" for other files).
func parentRel(t transcript) string {
	if t.main || t.session == "" {
		return ""
	}
	i := strings.Index(t.rel, "/")
	if i < 0 {
		return ""
	}
	return t.rel[:i] + "/" + t.session + ".jsonl"
}

// attribution is the decision scanHead's result allows: the harness of the
// session's Agent SDK lines and the owner of its first lines. ok is false
// while the decision is still open; settled forces it once the start of the
// file was scanned.
func (hi headInfo) attribution(parentSDK string, settled bool) (sdk, own string, ok bool) {
	open := hi.end && !hi.response && !hi.marker
	switch {
	case hi.more:
		return "", "", false
	case settled:
	case hi.entry == "" && open:
		return "", "", false
	case harnessEntry(hi.entry) && open && parentSDK != HarnessT3Code:
		return "", "", false
	}
	sdk = ownSelf
	if hi.marker || parentSDK == HarnessT3Code {
		sdk = HarnessT3Code
	}
	return sdk, lineOwner(hi.entry, sdk), true
}

func (f *fileState) sdk() string {
	if f == nil {
		return ""
	}
	return f.SDK
}
