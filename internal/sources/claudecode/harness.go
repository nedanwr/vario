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

	// headScanMax bounds how much of a transcript is read to attribute it.
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
	end      bool   // the whole file was scanned
}

// scanHead reads a transcript from the start up to its first response.
func scanHead(r io.Reader) (headInfo, error) {
	var hi headInfo
	br := bufio.NewReaderSize(io.LimitReader(r, headScanMax), 64<<10)
	var read int64
	for {
		line, complete, skipped, err := readLine(br)
		read += int64(len(line)) + skipped
		if complete {
			read++
		}
		if len(line) == 0 && !complete && skipped == 0 {
			if err != nil && err != io.EOF {
				return hi, err
			}
			hi.end = read < headScanMax
			return hi, nil
		}
		if !complete && !json.Valid(bytes.TrimSpace(line)) {
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

// attribute decides who runs the file f (see harness.go) and records it in
// fst. ok is false while the start of the file does not decide it yet; a
// file that was already partly read (by a version that did not attribute
// files) is decided with what it has.
func attribute(f io.ReaderAt, t transcript, fst, parent *fileState, mod time.Time) (bool, error) {
	hi, err := scanHead(io.NewSectionReader(f, 0, 1<<62))
	if err != nil {
		return false, err
	}
	settled := fst.Off > 0 || time.Since(mod) >= settleAfter
	if parentRel(t) != "" && parent.sdk() == "" && !settled {
		// A subagent inherits its session's attribution: wait for it.
		return false, nil
	}
	sdk, own, ok := hi.attribution(parent.sdk(), settled)
	if !ok {
		return false, nil
	}
	fst.SDK, fst.Own, fst.Own0 = sdk, own, own
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
// while the decision is still open; settled forces it.
func (hi headInfo) attribution(parentSDK string, settled bool) (sdk, own string, ok bool) {
	open := hi.end && !hi.response && !hi.marker
	switch {
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
