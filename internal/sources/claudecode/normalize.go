package claudecode

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/nedanwr/vario/internal/model"
)

// Provider is the provider recorded for Claude Code's responses: every
// transcript response is an Anthropic Messages API response (msg_ ids and
// Anthropic usage fields), whichever cloud served it.
const Provider = "anthropic"

// UsageSource identifies the usage measurement in provider_turns.
const UsageSource = "claudecode.message.usage"

// header is the part of a transcript line that is decoded for every line.
type header struct {
	Type             lstr            `json:"type"`
	Subtype          lstr            `json:"subtype"`
	UUID             lstr            `json:"uuid"`
	ParentUUID       lstr            `json:"parentUuid"`
	SessionID        lstr            `json:"sessionId"`
	Timestamp        lstr            `json:"timestamp"`
	IsSidechain      lbool           `json:"isSidechain"`
	AgentID          lstr            `json:"agentId"`
	IsMeta           lbool           `json:"isMeta"`
	IsCompactSummary lbool           `json:"isCompactSummary"`
	PromptID         lstr            `json:"promptId"`
	PromptSource     lstr            `json:"promptSource"`
	Cwd              lstr            `json:"cwd"`
	GitBranch        lstr            `json:"gitBranch"`
	Version          lstr            `json:"version"`
	Entrypoint       lstr            `json:"entrypoint"`
	UserType         lstr            `json:"userType"`
	PermissionMode   lstr            `json:"permissionMode"`
	RequestID        lstr            `json:"requestId"`
	RequestedModel   lstr            `json:"requestedModel"`
	IsAPIError       lbool           `json:"isApiErrorMessage"`
	Level            lstr            `json:"level"`
	CustomTitle      lstr            `json:"customTitle"`
	AITitle          lstr            `json:"aiTitle"`
	Summary          lstr            `json:"summary"`
	RetryAttempt     lint            `json:"retryAttempt"`
	MaxRetries       lint            `json:"maxRetries"`
	RetryInMs        lint            `json:"retryInMs"`
	Message          json.RawMessage `json:"message"`
	ToolUseResult    json.RawMessage `json:"toolUseResult"`
	Error            json.RawMessage `json:"error"`
	CompactMetadata  json.RawMessage `json:"compactMetadata"`
}

type message struct {
	ID         lstr            `json:"id"`
	Model      lstr            `json:"model"`
	Role       lstr            `json:"role"`
	Content    json.RawMessage `json:"content"`
	StopReason lstr            `json:"stop_reason"`
	Usage      lobj[usageJSON] `json:"usage"`
}

type usageJSON struct {
	InputTokens              lnum `json:"input_tokens"`
	CacheCreationInputTokens lnum `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     lnum `json:"cache_read_input_tokens"`
	OutputTokens             lnum `json:"output_tokens"`
	// Optional details are decoded on their own, so a malformed one is
	// ignored without losing the message or its other counts.
	OutputTokensDetails lobj[struct {
		ThinkingTokens lnum `json:"thinking_tokens"`
	}] `json:"output_tokens_details"`
	ServerToolUse lobj[struct {
		WebSearchRequests lnum `json:"web_search_requests"`
		WebFetchRequests  lnum `json:"web_fetch_requests"`
	}] `json:"server_tool_use"`
	CacheCreation lobj[struct {
		Ephemeral5m lnum `json:"ephemeral_5m_input_tokens"`
		Ephemeral1h lnum `json:"ephemeral_1h_input_tokens"`
	}] `json:"cache_creation"`
	ServiceTier lstr `json:"service_tier"`
	Speed       lstr `json:"speed"`
}

// usage is a response's token usage as reported. Claude Code repeats the
// usage on every line of a response (earlier versions wrote growing partial
// counts while streaming), so each field keeps the highest value seen.
type usage struct {
	In          *uint64 `json:"in,omitempty"`
	CacheCreate *uint64 `json:"cc,omitempty"`
	CacheRead   *uint64 `json:"cr,omitempty"`
	Out         *uint64 `json:"out,omitempty"`
	Thinking    *uint64 `json:"th,omitempty"`
	WebSearch   *uint64 `json:"ws,omitempty"`
	WebFetch    *uint64 `json:"wf,omitempty"`
	Cache5m     *uint64 `json:"c5,omitempty"`
	Cache1h     *uint64 `json:"c1,omitempty"`
	Tier        string  `json:"tier,omitempty"`
	Speed       string  `json:"speed,omitempty"`
}

func maxPtr(a *uint64, b lnum) *uint64 {
	if !b.ok {
		return a
	}
	if a == nil || b.v > *a {
		return b.ptr()
	}
	return a
}

func (u *usage) merge(j *usageJSON) {
	u.In = maxPtr(u.In, j.InputTokens)
	u.CacheCreate = maxPtr(u.CacheCreate, j.CacheCreationInputTokens)
	u.CacheRead = maxPtr(u.CacheRead, j.CacheReadInputTokens)
	u.Out = maxPtr(u.Out, j.OutputTokens)
	if d := j.OutputTokensDetails.v; d != nil {
		u.Thinking = maxPtr(u.Thinking, d.ThinkingTokens)
	}
	if s := j.ServerToolUse.v; s != nil {
		u.WebSearch = maxPtr(u.WebSearch, s.WebSearchRequests)
		u.WebFetch = maxPtr(u.WebFetch, s.WebFetchRequests)
	}
	if c := j.CacheCreation.v; c != nil {
		u.Cache5m = maxPtr(u.Cache5m, c.Ephemeral5m)
		u.Cache1h = maxPtr(u.Cache1h, c.Ephemeral1h)
	}
	if j.ServiceTier != "" {
		u.Tier = string(j.ServiceTier)
	}
	if j.Speed != "" {
		u.Speed = string(j.Speed)
	}
}

type block struct {
	Type      lstr            `json:"type"`
	Text      lstr            `json:"text"`
	Thinking  lstr            `json:"thinking"`
	ID        lstr            `json:"id"`
	Name      lstr            `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID lstr            `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   lbool           `json:"is_error"`
	Source    *struct {
		Type      lstr `json:"type"`
		MediaType lstr `json:"media_type"`
	} `json:"source"`
}

// blocks decodes message content, which is either a string or a list of blocks.
func blocks(raw json.RawMessage) []block {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return nil
	}
	if raw[0] == '"' {
		var s string
		if json.Unmarshal(raw, &s) != nil {
			return nil
		}
		return []block{{Type: "text", Text: lstr(s)}}
	}
	var out []json.RawMessage
	if json.Unmarshal(raw, &out) != nil {
		return nil
	}
	bs := make([]block, 0, len(out))
	for _, r := range out {
		var b block
		if json.Unmarshal(r, &b) == nil {
			bs = append(bs, b)
		}
	}
	return bs
}

// resultText renders tool_result content (a string or a list of blocks) as text.
func resultText(raw json.RawMessage) (string, int) {
	images := 0
	var parts []string
	for _, b := range blocks(raw) {
		switch b.Type {
		case "text":
			parts = append(parts, string(b.Text))
		case "image":
			images++
		}
	}
	return strings.Join(parts, "\n"), images
}

const (
	maxResponses = 2
	maxTools     = 256
)

// normalizer turns the lines of one transcript into records, updating the
// file's parse state as it goes.
type normalizer struct {
	s     *Source
	t     transcript
	fst   *fileState
	b     *batcher
	meta  *agentMeta
	dirty map[string]bool
	// quiet is set while lines that were already collected are read again
	// only to rebuild the parse state (see fileState.Compact).
	quiet bool
	// usageOnly is set while a line of a subagent run by T3 Code is read:
	// only its usage is collected (see harness.go).
	usageOnly bool
}

func (n *normalizer) add(r model.Record) {
	if n.quiet || (n.usageOnly && r.Table != model.TableProviderTurns) {
		return
	}
	n.b.add(r)
}

func newNormalizer(s *Source, t transcript, fst *fileState, b *batcher) *normalizer {
	n := &normalizer{s: s, t: t, fst: fst, b: b, dirty: map[string]bool{}}
	if !t.main && strings.HasSuffix(t.rel, ".jsonl") && strings.Contains(t.rel, "/subagents/") {
		n.meta = readAgentMeta(strings.TrimSuffix(t.path, ".jsonl") + ".meta.json")
	}
	return n
}

// agentMeta is the metadata Claude Code keeps next to a subagent transcript.
type agentMeta struct {
	AgentType   lstr  `json:"agentType"`
	IsBuiltIn   *bool `json:"isBuiltIn"`
	Description lstr  `json:"description"`
	ToolUseID   lstr  `json:"toolUseId"`
	SpawnDepth  lint  `json:"spawnDepth"`
	Model       lstr  `json:"model"`
}

func readAgentMeta(path string) *agentMeta {
	f, err := openShared(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var m agentMeta
	if json.NewDecoder(&limitedReader{f, 1 << 20}).Decode(&m) != nil {
		return nil
	}
	return &m
}

type limitedReader struct {
	f *os.File
	n int64
}

func (l *limitedReader) Read(p []byte) (int, error) {
	if l.n <= 0 {
		return 0, os.ErrInvalid
	}
	if int64(len(p)) > l.n {
		p = p[:l.n]
	}
	k, err := l.f.Read(p)
	l.n -= int64(k)
	return k, err
}

func (n *normalizer) policyText(s string) string {
	out, _, _ := n.s.policy.Text(s)
	return out
}

func (n *normalizer) env() model.Envelope {
	return model.Envelope{SourceID: n.s.id, Version: n.b.version}
}

func (n *normalizer) scoped(conv string, st *convState) model.Scoped {
	return model.Scoped{Envelope: n.env(), ConversationID: conv, ConversationCreatedAt: model.ParseTime(st.Created)}
}

func ptr[T any](v T) *T { return &v }

func later(a, b string) bool {
	ta, tb := model.ParseTime(a), model.ParseTime(b)
	return ta.After(tb.Time)
}

// line processes one complete transcript line that starts at offset off.
func (n *normalizer) line(off int64, raw []byte) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return
	}
	var h header
	if raw[0] != '{' || json.Unmarshal(raw, &h) != nil {
		n.fst.Bad++
		return
	}
	// A session file can contain lines copied from another session (for
	// example when a conversation is forked or resumed into a new session).
	// They are collected from their own session's file, never twice, and
	// leave this file's state alone.
	if n.t.main && h.SessionID != "" && string(h.SessionID) != n.t.session {
		return
	}
	ts := string(h.Timestamp)
	if ts != "" && !model.ParseTime(ts).IsZero() {
		n.fst.LastTS = ts
	} else {
		ts = ""
	}

	session := string(h.SessionID)
	if session == "" {
		session = n.t.session
	}
	if session == "" {
		session = strings.TrimSuffix(filepath.Base(n.t.path), ".jsonl")
	}
	agent := string(h.AgentID)
	if agent == "" {
		agent = n.t.agent
	}
	sub := bool(h.IsSidechain) || (!n.t.main && n.t.agent != "")
	conv := session
	if sub {
		if agent != "" {
			conv = session + "/agent-" + agent
		} else {
			conv = session + "/sidechain"
		}
	}

	// Lines run by T3 Code are collected from T3 Code; of its subagents'
	// lines only the usage is collected here (see harness.go).
	if h.Entrypoint != "" {
		n.fst.Own = lineOwner(string(h.Entrypoint), n.fst.SDK)
	}
	n.usageOnly = false
	if n.fst.Own == HarnessT3Code {
		if !sub {
			return
		}
		n.usageOnly = true
	}

	var msg *message
	if h.Type == "user" || h.Type == "assistant" {
		var m message
		if json.Unmarshal(h.Message, &m) == nil {
			msg = &m
		}
	}
	n.add(n.rawEvent(off, raw, &h, msg, conv, sub, session, agent))

	switch h.Type {
	case "user", "assistant", "system", "custom-title", "ai-title", "summary", "permission-mode", "queue-operation", "attachment":
	default:
		return
	}
	if (h.Type == "custom-title" || h.Type == "ai-title" || h.Type == "summary") && !sub {
		// Titles are not timestamped and belong to the session.
		conv = session
	}
	st := n.conv(conv)
	if st.Created == "" {
		if ts == "" {
			ts = n.fst.LastTS
		}
		if ts == "" {
			n.applyTitles(st, &h)
			return
		}
		st.Created = ts
		st.Updated = ts
		if sub {
			st.Sub = &subInfo{Parent: session}
		}
	}
	if ts != "" && later(ts, st.Updated) {
		st.Updated = ts
	}
	if st.Sub != nil && n.meta != nil {
		m := n.meta
		st.Sub.Type, st.Sub.BuiltIn, st.Sub.ParentTool, st.Sub.Model = string(m.AgentType), m.IsBuiltIn, string(m.ToolUseID), string(m.Model)
		if m.SpawnDepth.ok {
			st.Sub.Depth = ptr(m.SpawnDepth.v)
		}
		if n.s.policy.Messages() {
			st.Sub.Description = n.policyText(string(m.Description))
		}
	}
	if st.Sub != nil && agent != "" {
		st.Agent = agent
	}
	if h.Cwd != "" && st.Project == "" {
		st.Project = ProjectID(string(h.Cwd))
		n.add(n.project(st.Project, string(h.Cwd)))
	}
	if h.Version != "" {
		st.Version = string(h.Version)
	}
	if h.Entrypoint != "" {
		st.Entry = string(h.Entrypoint)
	}
	if h.UserType != "" {
		st.UserType = string(h.UserType)
	}
	if h.PermissionMode != "" {
		st.Perm = string(h.PermissionMode)
	}
	if h.GitBranch != "" && n.s.policy.Messages() {
		st.Branch = n.policyText(string(h.GitBranch))
	}
	n.applyTitles(st, &h)
	if !n.quiet && !n.usageOnly {
		n.dirty[conv] = true
	}

	switch h.Type {
	case "user":
		if msg != nil {
			n.user(conv, st, &h, msg, ts)
		}
	case "assistant":
		if msg != nil {
			n.assistant(conv, st, &h, msg, ts)
		}
	case "system":
		n.system(conv, st, &h, ts)
	}
}

func (n *normalizer) conv(id string) *convState {
	if n.fst.Convs == nil {
		n.fst.Convs = map[string]*convState{}
	}
	st := n.fst.Convs[id]
	if st == nil {
		st = &convState{}
		n.fst.Convs[id] = st
	}
	return st
}

func (n *normalizer) applyTitles(st *convState, h *header) {
	if !n.s.policy.Messages() {
		return
	}
	switch h.Type {
	case "custom-title":
		st.Custom = n.policyText(string(h.CustomTitle))
	case "ai-title":
		st.AITitle = n.policyText(string(h.AITitle))
	case "summary":
		st.Summary = n.policyText(string(h.Summary))
	}
}

// ProjectID derives a project identifier from a working directory without revealing it.
func ProjectID(cwd string) string {
	sum := sha256.Sum256([]byte(filepath.Clean(cwd)))
	return hex.EncodeToString(sum[:8])
}

func (n *normalizer) project(id, cwd string) model.Record {
	pr := model.ProjectRow{Envelope: n.env(), ProjectID: id}
	// Like T3 Code's project titles and paths, these are content.
	if n.s.policy.Messages() {
		pr.Title = n.policyText(filepath.Base(filepath.Clean(cwd)))
		pr.WorkspaceRoot = n.policyText(cwd)
	}
	return model.Record{Table: model.TableProjects, Key: id, Row: &pr}
}

// finish emits the conversations changed since the last call.
func (n *normalizer) finish() {
	ids := make([]string, 0, len(n.dirty))
	for id := range n.dirty {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		st := n.fst.Convs[id]
		if st != nil && st.Created != "" {
			n.add(n.conversation(id, st))
		}
	}
	clear(n.dirty)
}

func (n *normalizer) conversation(id string, st *convState) model.Record {
	c := model.ConversationRow{
		Envelope:       n.env(),
		ConversationID: id,
		ProjectID:      st.Project,
		Provider:       Provider,
		Model:          st.Model,
		RuntimeMode:    st.Perm,
		CreatedBy:      st.UserType,
		CreationSource: st.Entry,
		CreatedAt:      model.ParseTime(st.Created),
		UpdatedAt:      model.ParseTime(st.Updated),
	}
	attrs := map[string]any{}
	if st.Version != "" {
		attrs["claudeCodeVersion"] = st.Version
	}
	if s := st.Sub; s != nil {
		c.ParentConversationID = s.Parent
		c.RootConversationID = s.Parent
		c.Relationship = "subagent"
		if st.Agent != "" {
			attrs["agentId"] = st.Agent
		}
		if s.Type != "" {
			attrs["agentType"] = s.Type
		}
		if s.BuiltIn != nil {
			attrs["isBuiltIn"] = *s.BuiltIn
		}
		if s.Depth != nil {
			attrs["spawnDepth"] = *s.Depth
		}
		if s.ParentTool != "" {
			attrs["parentToolUseId"] = s.ParentTool
		}
		if s.Model != "" {
			attrs["requestedModel"] = s.Model
		}
	}
	if n.s.policy.Messages() {
		c.Title = first(st.Custom, st.AITitle, st.Summary)
		if st.Sub != nil {
			c.Title = first(st.Sub.Description, c.Title)
		}
		c.Branch = st.Branch
	}
	c.Attributes = n.attributes(attrs)
	return model.Record{Table: model.TableConversations, Key: id, Row: &c}
}

func first(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// attributes serializes metadata that is safe in every mode (identifiers,
// versions, counts and flags).
func (n *normalizer) attributes(m map[string]any) string {
	if len(m) == 0 {
		return ""
	}
	b, err := json.Marshal(m)
	if err != nil {
		return ""
	}
	s, _, _ := n.s.policy.JSON(string(b))
	return s
}

const interruptPrefix = "[Request interrupted by user"

func (n *normalizer) user(conv string, st *convState, h *header, msg *message, ts string) {
	bs := blocks(msg.Content)
	var texts []string
	var images []map[string]any
	results := false
	for _, b := range bs {
		switch b.Type {
		case "text":
			texts = append(texts, string(b.Text))
		case "image":
			img := map[string]any{"type": "image"}
			if b.Source != nil && b.Source.MediaType != "" {
				img["mimeType"] = string(b.Source.MediaType)
			}
			images = append(images, img)
		case "tool_result":
			results = true
			n.toolResult(conv, st, h, b, ts)
		}
	}
	if results || (len(texts) == 0 && len(images) == 0) {
		return
	}
	text := strings.Join(texts, "\n")
	source := string(h.PromptSource)
	interrupt := strings.HasPrefix(text, interruptPrefix)
	switch {
	case bool(h.IsCompactSummary):
		source = "compact_summary"
	case bool(h.IsMeta):
		source = "meta"
	case interrupt:
		source = "interrupt"
	}
	if interrupt {
		if st.Run != nil && st.Run.Status == "running" {
			st.Run.Status, st.Run.Completed = "interrupted", ts
			n.add(n.run(conv, st, st.Run))
		}
	} else if !h.IsMeta && !h.IsCompactSummary {
		id := first(string(h.PromptID), string(h.UUID))
		if st.Run == nil || st.Run.ID != id {
			if st.Run != nil && st.Run.Status == "running" {
				st.Run.Status = "incomplete"
				n.add(n.run(conv, st, st.Run))
			}
			st.Runs++
			st.Run = &runState{ID: id, Ord: st.Runs, UserMsg: string(h.UUID), Requested: ts, Status: "running"}
			n.add(n.run(conv, st, st.Run))
		}
	}
	if h.UUID == "" {
		return
	}
	m := model.MessageRow{
		Scoped:         n.scoped(conv, st),
		MessageID:      string(h.UUID),
		Role:           "user",
		CreationSource: source,
		TextChars:      uint64(utf8.RuneCountInString(text)),
		CreatedAt:      model.ParseTime(ts),
		UpdatedAt:      model.ParseTime(ts),
	}
	if st.Run != nil {
		m.RunID = st.Run.ID
	}
	n.messageContent(&m, text)
	if len(images) > 0 {
		m.AttachmentCount = uint32(len(images))
		b, _ := json.Marshal(images)
		m.Attachments = string(b)
	}
	n.add(model.Record{Table: model.TableMessages, Key: m.MessageID, Row: &m})
}

func (n *normalizer) messageContent(m *model.MessageRow, text string) {
	if !n.s.policy.Messages() {
		return
	}
	var red int
	var trunc bool
	m.Text, red, trunc = n.s.policy.Text(text)
	m.Redactions = uint32(red)
	m.ContentIncluded = 1
	if trunc {
		m.Truncated = 1
	}
}

func (n *normalizer) run(conv string, st *convState, r *runState) model.Record {
	row := model.RunRow{
		Scoped:        n.scoped(conv, st),
		RunID:         r.ID,
		Ordinal:       r.Ord,
		Provider:      Provider,
		Model:         r.Model,
		Status:        r.Status,
		UserMessageID: r.UserMsg,
		RequestedAt:   model.ParseTime(first(r.Requested, r.Started)),
		StartedAt:     model.ParseTime(r.Started),
		CompletedAt:   model.ParseTime(r.Completed),
	}
	return model.Record{Table: model.TableRuns, Key: r.ID, Row: &row}
}

func (n *normalizer) assistant(conv string, st *convState, h *header, msg *message, ts string) {
	mdl := string(msg.Model)
	synthetic := mdl == "<synthetic>" || bool(h.IsAPIError)
	respID := first(string(msg.ID), string(h.RequestID), string(h.UUID))
	if !synthetic {
		if mdl != "" {
			st.Model = mdl
		}
		if r := st.Run; r != nil {
			if r.Started == "" {
				r.Started = ts
			}
			if r.Model == "" {
				r.Model = first(string(h.RequestedModel), mdl)
			}
			switch stop := string(msg.StopReason); stop {
			case "", "tool_use", "pause_turn":
				r.Status, r.Completed = "running", ""
			default:
				r.Status, r.Completed = "completed", ts
			}
			n.add(n.run(conv, st, r))
		}
		if msg.Usage.v != nil {
			n.response(conv, st, h, msg, respID, ts)
		}
	}
	var texts []string
	for i, b := range blocks(msg.Content) {
		switch b.Type {
		case "text":
			texts = append(texts, string(b.Text))
		case "thinking", "redacted_thinking":
			n.reasoning(conv, st, h, respID, i, string(b.Thinking), ts)
		case "tool_use":
			n.toolUse(conv, st, b, respID, ts)
		}
	}
	if len(texts) == 0 || h.UUID == "" {
		return
	}
	text := strings.Join(texts, "\n")
	m := model.MessageRow{
		Scoped:    n.scoped(conv, st),
		MessageID: string(h.UUID),
		Role:      "assistant",
		TextChars: uint64(utf8.RuneCountInString(text)),
		CreatedAt: model.ParseTime(ts),
		UpdatedAt: model.ParseTime(ts),
	}
	if synthetic {
		m.CreationSource = "synthetic"
	}
	if st.Run != nil {
		m.RunID = st.Run.ID
	}
	n.messageContent(&m, text)
	n.add(model.Record{Table: model.TableMessages, Key: m.MessageID, Row: &m})
}

// response records one API response's usage. The response may span several
// lines, read in one or several reads; it is identified by its message id,
// so every line updates the same provider turn and nothing is counted twice.
func (n *normalizer) response(conv string, st *convState, h *header, msg *message, id, ts string) {
	var r *responseState
	for _, x := range st.Responses {
		if x.ID == id {
			r = x
			break
		}
	}
	if r == nil {
		st.Turns++
		r = &responseState{ID: id, Ord: st.Turns, Started: ts, Model: string(msg.Model), Request: string(h.RequestID)}
		if st.Run != nil {
			r.Run = st.Run.ID
		}
		st.Responses = append(st.Responses, r)
		if len(st.Responses) > maxResponses {
			st.Responses = st.Responses[len(st.Responses)-maxResponses:]
		}
	}
	if msg.StopReason != "" {
		r.Stop = string(msg.StopReason)
	}
	r.Usage.merge(msg.Usage.v)
	n.add(n.providerTurn(conv, st, r))
}

func addPtr(vals ...*uint64) *uint64 {
	var sum uint64
	for _, v := range vals {
		if v == nil {
			return nil
		}
		sum += *v
	}
	return &sum
}

func (n *normalizer) providerTurn(conv string, st *convState, r *responseState) model.Record {
	u := r.Usage
	t := model.ProviderTurnRow{
		Scoped:         n.scoped(conv, st),
		ProviderTurnID: r.ID,
		RunID:          r.Run,
		Ordinal:        r.Ord,
		Provider:       Provider,
		Model:          r.Model,
		Models:         []string{},
		StartedAt:      model.ParseTime(r.Started),
		UsageSource:    UsageSource,
		UsageScope:     "main_agent",
		// Anthropic reports input_tokens without cache reads and writes;
		// Vario's input_tokens is the total input, of which the cache
		// measurements are subsets.
		InputTokens:              addPtr(u.In, u.CacheCreate, u.CacheRead),
		CachedInputTokens:        u.CacheRead,
		CacheCreationInputTokens: u.CacheCreate,
		UncachedInputTokens:      u.In,
		OutputTokens:             u.Out,
		ReasoningTokens:          u.Thinking,
	}
	if st.Sub != nil {
		t.UsageScope = "subagent"
	}
	attrs := map[string]any{}
	if n.usageOnly {
		// The conversation and run are T3 Code's; only this usage is here.
		t.RunID = ""
		attrs["harness"] = HarnessT3Code
		if st.Agent != "" {
			attrs["agentId"] = st.Agent
		}
	}
	if r.Model != "" {
		t.Models = []string{r.Model}
		t.ModelSource = "usage"
	}
	switch r.Stop {
	case "":
		t.Status, t.UsageStatus = "incomplete", "partial"
	default:
		t.Status, t.UsageStatus = "completed", "complete"
	}
	if r.Stop != "" {
		attrs["stopReason"] = r.Stop
	}
	if r.Request != "" {
		attrs["requestId"] = r.Request
	}
	if u.Tier != "" {
		attrs["serviceTier"] = u.Tier
	}
	if u.Speed != "" {
		attrs["speed"] = u.Speed
	}
	if u.WebSearch != nil {
		attrs["webSearchRequests"] = *u.WebSearch
	}
	if u.WebFetch != nil {
		attrs["webFetchRequests"] = *u.WebFetch
	}
	if u.Cache5m != nil {
		attrs["cacheCreation5mTokens"] = *u.Cache5m
	}
	if u.Cache1h != nil {
		attrs["cacheCreation1hTokens"] = *u.Cache1h
	}
	t.Attributes = n.attributes(attrs)
	return model.Record{Table: model.TableProviderTurns, Key: r.ID, Row: &t}
}

func (n *normalizer) reasoning(conv string, st *convState, h *header, resp string, idx int, text, ts string) {
	if h.UUID == "" {
		return
	}
	st.Events++
	e := model.ExecutionEventRow{
		Scoped:         n.scoped(conv, st),
		EventID:        string(h.UUID) + "#" + strconv.Itoa(idx),
		ProviderTurnID: resp,
		Ordinal:        st.Events,
		EventType:      "reasoning",
		Status:         "completed",
		OutputBytes:    uint64(len(text)),
		StartedAt:      model.ParseTime(ts),
		CompletedAt:    model.ParseTime(ts),
		UpdatedAt:      model.ParseTime(ts),
	}
	if st.Run != nil {
		e.RunID = st.Run.ID
	}
	if n.s.policy.Full() {
		var red int
		var trunc bool
		e.Output, red, trunc = n.s.policy.Text(text)
		e.Redactions, e.Truncated, e.ContentIncluded = uint32(red), boolToUint8(trunc), 1
	}
	n.add(model.Record{Table: model.TableExecutionEvents, Key: e.EventID, Row: &e})
}

// eventType maps Claude Code tool names onto Vario's execution event types.
func eventType(tool string) string {
	switch tool {
	case "Bash", "PowerShell":
		return "command_execution"
	case "Edit", "MultiEdit", "Write", "NotebookEdit":
		return "file_change"
	case "WebSearch", "WebFetch":
		return "web_search"
	}
	return "dynamic_tool"
}

func (n *normalizer) toolUse(conv string, st *convState, b block, resp, ts string) {
	id := string(b.ID)
	if id == "" {
		return
	}
	name := string(b.Name)
	var in map[string]json.RawMessage
	_ = json.Unmarshal(b.Input, &in)
	str := func(k string) string {
		var s string
		if v, ok := in[k]; ok && json.Unmarshal(v, &s) == nil {
			return s
		}
		return ""
	}
	tool := &toolState{Name: name, Type: eventType(name), Turn: resp, Started: ts, InBytes: uint64(len(bytes.TrimSpace(b.Input)))}
	if prev := st.Tools[id]; prev != nil {
		tool.Ord = prev.Ord
	} else {
		st.Events++
		tool.Ord = st.Events
	}
	if st.Run != nil {
		tool.Run = st.Run.ID
	}
	attrs := map[string]any{}
	if t := str("subagent_type"); t != "" {
		attrs["subagentType"] = t
	}
	if v, ok := in["run_in_background"]; ok && string(v) == "true" {
		attrs["background"] = true
	}
	tool.Attrs = n.attributes(attrs)
	if tool.Type == "file_change" && n.s.policy.Messages() {
		tool.FileName = n.policyText(first(str("file_path"), str("notebook_path")))
	}
	if n.s.policy.Full() {
		var red int
		var trunc bool
		tool.Input, red, trunc = n.s.policy.Text(string(bytes.TrimSpace(b.Input)))
		tool.Title = n.policyText(str("description"))
		tool.Redact, tool.Truncated = uint32(red), trunc
	}
	if st.Tools == nil {
		st.Tools = map[string]*toolState{}
	}
	if st.Tools[id] == nil {
		st.ToolOrder = append(st.ToolOrder, id)
	}
	st.Tools[id] = tool
	for len(st.ToolOrder) > maxTools {
		delete(st.Tools, st.ToolOrder[0])
		st.ToolOrder = st.ToolOrder[1:]
	}
	n.add(n.toolEvent(conv, st, id, tool, nil))
}

// toolOutcome is a tool call's result.
type toolOutcome struct {
	ts      string
	status  string
	isError bool
	output  string
	images  int
}

func (n *normalizer) toolResult(conv string, st *convState, h *header, b block, ts string) {
	id := string(b.ToolUseID)
	tool := st.Tools[id]
	if tool == nil {
		// The call was not seen (it is in a file read before collection
		// started, or was evicted); keep whatever was recorded for it.
		return
	}
	delete(st.Tools, id)
	for i, x := range st.ToolOrder {
		if x == id {
			st.ToolOrder = append(st.ToolOrder[:i], st.ToolOrder[i+1:]...)
			break
		}
	}
	out := &toolOutcome{ts: ts, status: "completed", isError: bool(b.IsError)}
	out.output, out.images = resultText(b.Content)
	var res struct {
		Interrupted lbool `json:"interrupted"`
	}
	if len(h.ToolUseResult) > 0 && h.ToolUseResult[0] == '{' {
		_ = json.Unmarshal(h.ToolUseResult, &res)
	}
	switch {
	case bool(res.Interrupted):
		out.status = "interrupted"
	case out.isError:
		out.status = "failed"
	}
	n.add(n.toolEvent(conv, st, id, tool, out))
}

func (n *normalizer) toolEvent(conv string, st *convState, id string, t *toolState, out *toolOutcome) model.Record {
	e := model.ExecutionEventRow{
		Scoped:         n.scoped(conv, st),
		EventID:        id,
		RunID:          t.Run,
		ProviderTurnID: t.Turn,
		Ordinal:        t.Ord,
		EventType:      t.Type,
		Status:         "running",
		ToolName:       t.Name,
		InputBytes:     t.InBytes,
		FileName:       t.FileName,
		StartedAt:      model.ParseTime(t.Started),
		UpdatedAt:      model.ParseTime(t.Started),
		Attributes:     t.Attrs,
	}
	if n.s.policy.Full() {
		e.Input, e.Title, e.ContentIncluded = t.Input, t.Title, 1
		e.Redactions, e.Truncated = t.Redact, boolToUint8(t.Truncated)
	}
	if out != nil {
		e.Status = out.status
		e.IsError = boolToUint8(out.isError)
		e.OutputBytes = uint64(len(out.output))
		e.CompletedAt = model.ParseTime(out.ts)
		e.UpdatedAt = model.ParseTime(first(out.ts, t.Started))
		if !e.StartedAt.IsZero() && !e.CompletedAt.IsZero() && !e.CompletedAt.Before(e.StartedAt.Time) {
			e.DurationMs = ptr(e.CompletedAt.Sub(e.StartedAt.Time).Milliseconds())
		}
		if n.s.policy.Full() {
			o, red, trunc := n.s.policy.Text(out.output)
			e.Output = o
			e.Redactions += uint32(red)
			e.Truncated = boolToUint8(e.Truncated == 1 || trunc)
		}
	}
	return model.Record{Table: model.TableExecutionEvents, Key: id, Row: &e}
}

func boolToUint8(b bool) uint8 {
	if b {
		return 1
	}
	return 0
}

func (n *normalizer) system(conv string, st *convState, h *header, ts string) {
	if h.UUID == "" {
		return
	}
	var typ, status, output string
	attrs := map[string]any{}
	isErr := false
	switch h.Subtype {
	case "api_error":
		typ, status, isErr = "error", "failed", true
		if h.RetryAttempt.ok {
			attrs["retryAttempt"] = h.RetryAttempt.v
		}
		if h.MaxRetries.ok {
			attrs["maxRetries"] = h.MaxRetries.v
		}
		if h.RetryInMs.ok {
			attrs["retryInMs"] = h.RetryInMs.v
		}
		var e struct {
			Status lint `json:"status"`
		}
		if len(h.Error) > 0 && h.Error[0] == '{' && json.Unmarshal(h.Error, &e) == nil && e.Status.ok {
			attrs["httpStatus"] = e.Status.v
		}
		output = string(h.Error)
	case "compact_boundary":
		typ, status = "context_compaction", "completed"
		var m struct {
			Trigger    lstr `json:"trigger"`
			PreTokens  lnum `json:"preTokens"`
			PostTokens lnum `json:"postTokens"`
			DurationMs lnum `json:"durationMs"`
		}
		if len(h.CompactMetadata) > 0 && json.Unmarshal(h.CompactMetadata, &m) == nil {
			if m.Trigger != "" {
				attrs["trigger"] = string(m.Trigger)
			}
			if m.PreTokens.ok {
				attrs["preTokens"] = m.PreTokens.v
			}
			if m.PostTokens.ok {
				attrs["postTokens"] = m.PostTokens.v
			}
			if m.DurationMs.ok {
				attrs["durationMs"] = m.DurationMs.v
			}
		}
	default:
		return
	}
	st.Events++
	e := model.ExecutionEventRow{
		Scoped:      n.scoped(conv, st),
		EventID:     string(h.UUID),
		Ordinal:     st.Events,
		EventType:   typ,
		Status:      status,
		IsError:     boolToUint8(isErr),
		OutputBytes: uint64(len(output)),
		StartedAt:   model.ParseTime(ts),
		CompletedAt: model.ParseTime(ts),
		UpdatedAt:   model.ParseTime(ts),
		Attributes:  n.attributes(attrs),
	}
	if st.Run != nil {
		e.RunID = st.Run.ID
	}
	if n.s.policy.Full() && output != "" {
		var red int
		var trunc bool
		e.Output, red, trunc = n.s.policy.Text(output)
		e.Redactions, e.Truncated, e.ContentIncluded = uint32(red), boolToUint8(trunc), 1
	}
	n.add(model.Record{Table: model.TableExecutionEvents, Key: e.EventID, Row: &e})
}

// rawEvent records the line itself. Its payload is only kept in full mode;
// the metadata holds identifiers and versions only.
func (n *normalizer) rawEvent(off int64, raw []byte, h *header, msg *message, conv string, sub bool, session, agent string) model.Record {
	ts := model.ParseTime(first(string(h.Timestamp), n.fst.LastTS))
	id := string(h.UUID)
	if id == "" {
		id = session
		if sub && agent != "" {
			id += "/agent-" + agent
		}
		id += "@" + strconv.FormatInt(off, 10)
	}
	ev := model.RawEventRow{
		Envelope:      n.env(),
		EventID:       id,
		AggregateKind: "session",
		StreamID:      conv,
		EventType:     string(h.Type),
		OccurredAt:    ts,
	}
	if !ts.IsZero() {
		ev.Sequence = ts.UnixMilli()
	}
	if sub {
		ev.AggregateKind = "subagent"
	}
	if h.Subtype != "" {
		ev.EventType += "." + string(h.Subtype)
	}
	meta := map[string]any{}
	set := func(k string, v lstr) {
		if v != "" {
			meta[k] = string(v)
		}
	}
	set("version", h.Version)
	set("entrypoint", h.Entrypoint)
	set("userType", h.UserType)
	set("agentId", h.AgentID)
	set("level", h.Level)
	set("parentUuid", h.ParentUUID)
	set("promptId", h.PromptID)
	set("requestId", h.RequestID)
	set("permissionMode", h.PermissionMode)
	if h.IsSidechain {
		meta["isSidechain"] = true
	}
	if h.IsMeta {
		meta["isMeta"] = true
	}
	if msg != nil {
		set("messageId", msg.ID)
		set("model", msg.Model)
		set("stopReason", msg.StopReason)
	}
	ev.Metadata = n.attributes(meta)
	// Account identifiers are not conversation telemetry; they are never copied.
	if n.s.policy.Full() && h.Type != "session-owner" {
		var dropped bool
		ev.Payload, _, dropped = n.s.policy.JSON(string(raw))
		ev.PayloadIncluded = boolToUint8(!dropped && ev.Payload != "")
	}
	return model.Record{Table: model.TableRawEvents, Key: strconv.FormatInt(ev.Sequence, 10) + "/" + ev.EventID, Row: &ev}
}
