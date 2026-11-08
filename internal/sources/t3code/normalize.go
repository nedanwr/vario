package t3code

import (
	"encoding/json"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/nedanwr/vario/internal/model"
	"github.com/nedanwr/vario/internal/privacy"
)

// payload is a decoded JSON object whose values stay raw until accessed.
type payload map[string]json.RawMessage

func parsePayload(s string) payload {
	var p payload
	if s == "" || json.Unmarshal([]byte(s), &p) != nil {
		return payload{}
	}
	return p
}

func (p payload) str(k string) string {
	var s string
	if v, ok := p[k]; ok && json.Unmarshal(v, &s) == nil {
		return s
	}
	return ""
}

func (p payload) obj(k string) payload {
	var o payload
	if v, ok := p[k]; ok && json.Unmarshal(v, &o) == nil {
		return o
	}
	return nil
}

func (p payload) boolean(k string) *bool {
	var b bool
	if v, ok := p[k]; ok && json.Unmarshal(v, &b) == nil {
		return &b
	}
	return nil
}

func (p payload) uint(k string) *uint64 {
	v, ok := p[k]
	if !ok {
		return nil
	}
	s := string(v)
	if n, err := strconv.ParseUint(s, 10, 64); err == nil {
		return &n
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil && f >= 0 && f == math.Trunc(f) && f < math.MaxUint64 {
		n := uint64(f)
		return &n
	}
	return nil
}

func (p payload) int(k string) *int64 {
	v, ok := p[k]
	if !ok {
		return nil
	}
	s := string(v)
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		return &n
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil && f == math.Trunc(f) && math.Abs(f) < math.MaxInt64 {
		n := int64(f)
		return &n
	}
	return nil
}

func (p payload) strings(k string) []string {
	var ss []string
	if v, ok := p[k]; ok && json.Unmarshal(v, &ss) == nil {
		return ss
	}
	return nil
}

// text renders a JSON value as text: strings unquoted, anything else as compact JSON.
func (p payload) text(k string) string {
	v, ok := p[k]
	if !ok || string(v) == "null" {
		return ""
	}
	var s string
	if json.Unmarshal(v, &s) == nil {
		return s
	}
	return string(v)
}

func first(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func set(keys ...string) map[string]bool {
	m := make(map[string]bool, len(keys))
	for _, k := range keys {
		m[k] = true
	}
	return m
}

// normalizer converts T3 rows into model rows for one snapshot version.
type normalizer struct {
	sourceID      string
	version       uint64
	policy        *privacy.Policy
	threadCreated map[string]model.Time
}

func (n *normalizer) env() model.Envelope {
	return model.Envelope{SourceID: n.sourceID, Version: n.version}
}

func (n *normalizer) scoped(threadID string) model.Scoped {
	return model.Scoped{Envelope: n.env(), ConversationID: threadID, ConversationCreatedAt: n.threadCreated[threadID]}
}

// attributes keeps payload keys not mapped to columns. Outside full mode only
// keys in keep are retained; drop is always removed.
func (n *normalizer) attributes(p payload, drop, keep map[string]bool, extra map[string]any) string {
	out := map[string]any{}
	for k, v := range p {
		if drop[k] || string(v) == "null" {
			continue
		}
		if keep[k] || n.policy.Full() {
			out[k] = v
		}
	}
	for k, v := range extra {
		out[k] = v
	}
	if len(out) == 0 {
		return ""
	}
	b, err := json.Marshal(out)
	if err != nil {
		return ""
	}
	if n.policy.Full() {
		s, _, _ := n.policy.JSON(string(b))
		return s
	}
	return string(b)
}

var (
	threadDrop = set("id", "projectId", "title", "providerInstanceId", "runtimeMode", "interactionMode", "branch", "lineage",
		"createdAt", "updatedAt", "archivedAt", "deletedAt", "createdBy", "creationSource",
		// Volatile UI state that would otherwise create a new version on every visit.
		"lastVisitedAt", "opens", "pinOrderKey", "activeOrderKey", "titleRegeneration")
	threadKeepMetrics  = set("modelSelection", "activeProviderThreadId", "forkedFrom", "historyOrigin", "settledAt", "settledOverride", "unsettledAt", "snoozedUntil", "snoozedAt", "pinnedAt", "autoSettleDisabledAt")
	threadKeepMessages = set("worktreePath", "pullRequests", "linkedPullRequest", "branchPullRequest")
)

func (n *normalizer) conversation(r row) model.ConversationRow {
	p := parsePayload(r.get("payload_json"))
	lineage := p.obj("lineage")
	keep := threadKeepMetrics
	if n.policy.Messages() {
		keep = mergeSets(threadKeepMetrics, threadKeepMessages)
	}
	c := model.ConversationRow{
		Envelope:             n.env(),
		ConversationID:       r.get("thread_id"),
		ProjectID:            first(r.get("project_id"), p.str("projectId")),
		Provider:             first(r.get("default_provider"), p.obj("modelSelection").str("instanceId")),
		ProviderInstanceID:   first(r.get("provider_instance_id"), p.str("providerInstanceId")),
		Model:                p.obj("modelSelection").str("model"),
		RuntimeMode:          first(r.get("runtime_mode"), p.str("runtimeMode")),
		InteractionMode:      first(r.get("interaction_mode"), p.str("interactionMode")),
		ParentConversationID: lineage.str("parentThreadId"),
		RootConversationID:   lineage.str("rootThreadId"),
		Relationship:         lineage.str("relationshipToParent"),
		CreatedBy:            p.str("createdBy"),
		CreationSource:       p.str("creationSource"),
		CreatedAt:            model.ParseTime(first(r.get("created_at"), p.str("createdAt"))),
		UpdatedAt:            model.ParseTime(first(r.get("updated_at"), p.str("updatedAt"))),
		ArchivedAt:           model.ParseTime(first(r.get("archived_at"), p.str("archivedAt"))),
		DeletedAt:            model.ParseTime(first(r.get("deleted_at"), p.str("deletedAt"))),
		Attributes:           n.attributes(p, threadDrop, keep, nil),
	}
	if !c.DeletedAt.IsZero() {
		c.IsDeleted = 1
	}
	if n.policy.Messages() {
		c.Title, _, _ = n.policy.Text(first(r.get("title"), p.str("title")))
		c.Branch, _, _ = n.policy.Text(p.str("branch"))
	}
	return c
}

func mergeSets(sets ...map[string]bool) map[string]bool {
	out := map[string]bool{}
	for _, s := range sets {
		for k := range s {
			out[k] = true
		}
	}
	return out
}

func (n *normalizer) project(r row) model.ProjectRow {
	pr := model.ProjectRow{
		Envelope:  n.env(),
		ProjectID: r.get("project_id"),
		Title:     r.get("title"),
		CreatedAt: model.ParseTime(r.get("created_at")),
		UpdatedAt: model.ParseTime(r.get("updated_at")),
		DeletedAt: model.ParseTime(r.get("deleted_at")),
	}
	if !pr.DeletedAt.IsZero() {
		pr.IsDeleted = 1
	}
	if n.policy.Messages() {
		pr.WorkspaceRoot = r.get("workspace_root")
	}
	return pr
}

var (
	runDrop = set("id", "threadId", "ordinal", "providerInstanceId", "modelSelection", "userMessageId", "status",
		"requestedAt", "startedAt", "completedAt")
	runKeep = set("providerThreadId", "rootNodeId", "activeAttemptId", "checkpointId", "contextHandoffId", "queuePosition")
)

func (n *normalizer) run(r row) model.RunRow {
	p := parsePayload(r.get("payload_json"))
	sel := p.obj("modelSelection")
	threadID := first(r.get("thread_id"), p.str("threadId"))
	rr := model.RunRow{
		Scoped:             n.scoped(threadID),
		RunID:              r.get("run_id"),
		Provider:           first(r.get("provider"), sel.str("instanceId")),
		ProviderInstanceID: first(r.get("provider_instance_id"), p.str("providerInstanceId")),
		Model:              sel.str("model"),
		Status:             first(r.get("status"), p.str("status")),
		UserMessageID:      p.str("userMessageId"),
		RequestedAt:        model.ParseTime(first(r.get("requested_at"), p.str("requestedAt"))),
		StartedAt:          model.ParseTime(p.str("startedAt")),
		CompletedAt:        model.ParseTime(first(r.get("completed_at"), p.str("completedAt"))),
		Attributes:         n.attributes(p, runDrop, runKeep, nil),
	}
	if v, ok := sel["options"]; ok {
		rr.ModelOptions = string(v)
	}
	if o := parseInt(r.get("ordinal")); o != nil {
		rr.Ordinal = *o
	} else if o := p.int("ordinal"); o != nil {
		rr.Ordinal = *o
	}
	return rr
}

func parseInt(s string) *int64 {
	if s == "" {
		return nil
	}
	if v, err := strconv.ParseInt(s, 10, 64); err == nil {
		return &v
	}
	return nil
}

func (n *normalizer) message(r row) model.MessageRow {
	p := parsePayload(r.get("payload_json"))
	threadID := first(r.get("thread_id"), p.str("threadId"))
	text := p.str("text")
	m := model.MessageRow{
		Scoped:         n.scoped(threadID),
		MessageID:      r.get("message_id"),
		RunID:          first(r.get("run_id"), p.str("runId")),
		NodeID:         first(r.get("node_id"), p.str("nodeId")),
		Role:           first(r.get("role"), p.str("role")),
		CreatedBy:      p.str("createdBy"),
		CreationSource: p.str("creationSource"),
		TextChars:      uint64(utf8.RuneCountInString(text)),
		CreatedAt:      model.ParseTime(first(r.get("created_at"), p.str("createdAt"))),
		UpdatedAt:      model.ParseTime(first(r.get("updated_at"), p.str("updatedAt"))),
	}
	if s := r.get("streaming"); s == "1" || (s == "" && p.boolean("streaming") != nil && *p.boolean("streaming")) {
		m.Streaming = 1
	}
	if n.policy.Messages() {
		var red int
		var trunc bool
		m.Text, red, trunc = n.policy.Text(text)
		m.Redactions = uint32(red)
		m.ContentIncluded = 1
		if trunc {
			m.Truncated = 1
		}
	}
	m.AttachmentCount, m.Attachments = n.attachments(p["attachments"])
	return m
}

// attachments returns the attachment count and their metadata. File names and
// ids are only kept when message content is collected.
func (n *normalizer) attachments(raw json.RawMessage) (uint32, string) {
	var list []payload
	if len(raw) == 0 || json.Unmarshal(raw, &list) != nil || len(list) == 0 {
		return 0, ""
	}
	if n.policy.Messages() {
		s, _, _ := n.policy.JSON(string(raw))
		return uint32(len(list)), s
	}
	safe := make([]map[string]any, 0, len(list))
	for _, a := range list {
		m := map[string]any{}
		if t := a.str("type"); t != "" {
			m["type"] = t
		}
		if t := a.str("mimeType"); t != "" {
			m["mimeType"] = t
		}
		if sz := a.uint("sizeBytes"); sz != nil {
			m["sizeBytes"] = *sz
		}
		safe = append(safe, m)
	}
	b, _ := json.Marshal(safe)
	return uint32(len(list)), string(b)
}

var (
	itemDrop = set("id", "threadId", "runId", "nodeId", "providerThreadId", "providerTurnId", "parentItemId", "ordinal", "status",
		"title", "startedAt", "completedAt", "updatedAt", "type", "messageId", "toolName", "exitCode", "fileName",
		// Content extracted into input/output, or owned by the messages table.
		"text", "input", "output", "diffStr", "patterns", "results", "failure", "summary", "detail", "message",
		"questions", "questionAnswer", "files", "attachments")
	itemKeep = set("nativeItemRef", "createdBy", "creationSource", "inputIntent", "streaming", "driver",
		"beforeTokenCount", "afterTokenCount", "source", "outcome", "checkpointId", "scopeId", "requestId", "retry", "idleCompaction")
)

func (n *normalizer) executionEvent(r row) model.ExecutionEventRow {
	p := parsePayload(r.get("payload_json"))
	threadID := first(r.get("thread_id"), p.str("threadId"))
	typ := first(r.get("type"), p.str("type"))
	e := model.ExecutionEventRow{
		Scoped:           n.scoped(threadID),
		EventID:          r.get("turn_item_id"),
		RunID:            first(r.get("run_id"), p.str("runId")),
		NodeID:           first(r.get("node_id"), p.str("nodeId")),
		ProviderThreadID: first(r.get("provider_thread_id"), p.str("providerThreadId")),
		ProviderTurnID:   first(r.get("provider_turn_id"), p.str("providerTurnId")),
		ParentEventID:    first(r.get("parent_item_id"), p.str("parentItemId")),
		EventType:        typ,
		Status:           first(r.get("status"), p.str("status")),
		StartedAt:        model.ParseTime(p.str("startedAt")),
		CompletedAt:      model.ParseTime(p.str("completedAt")),
		UpdatedAt:        model.ParseTime(first(r.get("updated_at"), p.str("updatedAt"))),
	}
	if o := parseInt(r.get("ordinal")); o != nil {
		e.Ordinal = *o
	} else if o := p.int("ordinal"); o != nil {
		e.Ordinal = *o
	}
	if !e.StartedAt.IsZero() && !e.CompletedAt.IsZero() && !e.CompletedAt.Before(e.StartedAt.Time) {
		d := e.CompletedAt.Sub(e.StartedAt.Time).Milliseconds()
		e.DurationMs = &d
	}

	var input, output string
	var extra map[string]any
	switch typ {
	case "user_message", "assistant_message":
		e.MessageID = p.str("messageId")
	case "command_execution":
		input, output = p.text("input"), p.text("output")
		e.ExitCode = p.int("exitCode")
	case "dynamic_tool":
		e.ToolName = p.str("toolName")
		input, output = p.text("input"), p.text("output")
	case "reasoning":
		output = p.text("text")
	case "file_change":
		input = p.text("diffStr")
		if n.policy.Messages() {
			e.FileName = p.str("fileName")
		}
	case "web_search":
		input, output = p.text("patterns"), p.text("results")
	case "error":
		output = p.text("failure")
		if f := p.obj("failure"); f != nil {
			meta := map[string]any{}
			for _, k := range []string{"class", "code"} {
				if v := f.str(k); v != "" {
					meta[k] = v
				}
			}
			if b := f.boolean("retryable"); b != nil {
				meta["retryable"] = *b
			}
			if !n.policy.Full() && len(meta) > 0 {
				extra = map[string]any{"failure": meta}
			}
		}
	case "notification":
		output = strings.TrimSpace(p.text("summary") + "\n" + p.text("detail"))
	case "system_notice", "run_interrupt_request":
		output = p.text("message")
	case "user_input_request":
		input, output = p.text("questions"), p.text("questionAnswer")
	case "checkpoint":
		output = p.text("files")
	}
	e.InputBytes, e.OutputBytes = uint64(len(input)), uint64(len(output))
	e.IsError = boolToUint8(e.Status == "failed" || typ == "error" || (e.ExitCode != nil && *e.ExitCode != 0))

	if n.policy.Full() {
		var r1, r2 int
		var t1, t2 bool
		e.Input, r1, t1 = n.policy.Text(input)
		e.Output, r2, t2 = n.policy.Text(output)
		e.Title, _, _ = n.policy.Text(p.str("title"))
		e.Redactions = uint32(r1 + r2)
		e.Truncated = boolToUint8(t1 || t2)
		e.ContentIncluded = 1
	}
	e.Attributes = n.attributes(p, itemDrop, itemKeep, extra)
	return e
}

func boolToUint8(b bool) uint8 {
	if b {
		return 1
	}
	return 0
}

type runInfo struct {
	runID, provider, providerInstanceID, model string
}

var (
	turnDrop = set("id", "providerThreadId", "nodeId", "runAttemptId", "ordinal", "status", "startedAt", "completedAt")
	// Raw usage objects are kept for provenance; they contain no conversation content.
	turnKeep = set("nativeTurnRef", "tokenUsage", "turnTokenUsage")
)

func (n *normalizer) providerTurn(r row, run runInfo) model.ProviderTurnRow {
	p := parsePayload(r.get("payload_json"))
	t := model.ProviderTurnRow{
		Scoped:             n.scoped(r.get("thread_id")),
		ProviderTurnID:     r.get("provider_turn_id"),
		ProviderThreadID:   first(r.get("provider_thread_id"), p.str("providerThreadId")),
		RunID:              run.runID,
		RunAttemptID:       first(r.get("run_attempt_id"), p.str("runAttemptId")),
		NodeID:             first(r.get("node_id"), p.str("nodeId")),
		Status:             first(r.get("status"), p.str("status")),
		Provider:           first(run.provider, p.obj("nativeTurnRef").str("driver")),
		ProviderInstanceID: run.providerInstanceID,
		StartedAt:          model.ParseTime(first(r.get("started_at"), p.str("startedAt"))),
		CompletedAt:        model.ParseTime(first(r.get("completed_at"), p.str("completedAt"))),
		Attributes:         n.attributes(p, turnDrop, turnKeep, nil),
	}
	if o := parseInt(r.get("ordinal")); o != nil {
		t.Ordinal = *o
	} else if o := p.int("ordinal"); o != nil {
		t.Ordinal = *o
	}
	if !t.StartedAt.IsZero() && !t.CompletedAt.IsZero() && !t.CompletedAt.Before(t.StartedAt.Time) {
		d := t.CompletedAt.Sub(t.StartedAt.Time).Milliseconds()
		t.DurationMs = &d
	}
	applyTurnUsage(&t, p.obj("turnTokenUsage"))
	applyContextUsage(&t, p.obj("tokenUsage"))
	if len(t.Models) > 0 {
		t.Model, t.ModelSource = t.Models[0], "usage"
	} else if run.model != "" {
		t.Model, t.ModelSource = run.model, "run"
	}
	if t.Models == nil {
		t.Models = []string{}
	}
	return t
}

// applyTurnUsage maps turnTokenUsage, the provider-reported consumption of the
// turn. Values are copied as reported: input already includes cached and
// cache-creation tokens, and reasoning is not added to output.
func applyTurnUsage(t *model.ProviderTurnRow, u payload) {
	if u == nil {
		return
	}
	t.UsageSource = "t3code.turnTokenUsage"
	t.UsageScope = u.str("usageScope")
	t.UsageStatus = u.str("usageStatus")
	if b := u.boolean("hasSubagents"); b != nil {
		v := boolToUint8(*b)
		t.HasSubagents = &v
	}
	t.InputTokens = u.uint("inputTokens")
	t.CachedInputTokens = u.uint("cachedInputTokens")
	t.CacheCreationInputTokens = u.uint("cacheCreationTokens")
	t.OutputTokens = u.uint("outputTokens")
	t.ReasoningTokens = u.uint("reasoningTokens")
	t.Models = u.strings("models")
	if other := u.obj("otherUsage"); other != nil {
		t.OtherInputTokens = other.uint("inputTokens")
		t.OtherOutputTokens = other.uint("outputTokens")
	}
	t.UncachedInputTokens = uncached(t.InputTokens, t.CachedInputTokens, t.CacheCreationInputTokens)
}

// uncached derives non-cache input tokens only when every component is reported
// and consistent with input being a superset of the cache measurements.
func uncached(input, cached, creation *uint64) *uint64 {
	if input == nil || cached == nil || creation == nil {
		return nil
	}
	if *cached+*creation > *input {
		return nil
	}
	v := *input - *cached - *creation
	return &v
}

// applyContextUsage maps tokenUsage, a snapshot of context window occupancy.
func applyContextUsage(t *model.ProviderTurnRow, u payload) {
	if u == nil {
		return
	}
	t.ContextUsedTokens = u.uint("usedTokens")
	t.ContextMaxTokens = u.uint("maxTokens")
	t.ContextInputTokens = u.uint("inputTokens")
	t.ContextCachedInputTokens = u.uint("cachedInputTokens")
	t.ContextOutputTokens = u.uint("outputTokens")
	t.ContextReasoningOutputTokens = u.uint("reasoningOutputTokens")
	t.ContextUpdatedAt = model.ParseTime(u.str("updatedAt"))
}

func (n *normalizer) rawEvent(r row) model.RawEventRow {
	ev := model.RawEventRow{
		Envelope:      n.env(),
		EventID:       r.get("event_id"),
		AggregateKind: r.get("aggregate_kind"),
		StreamID:      r.get("stream_id"),
		EventType:     r.get("event_type"),
		OccurredAt:    model.ParseTime(r.get("occurred_at")),
		Metadata:      r.get("metadata_json"),
	}
	if s := parseInt(r.get("sequence")); s != nil {
		ev.Sequence = *s
	}
	if !json.Valid([]byte(ev.Metadata)) {
		ev.Metadata = ""
	}
	if n.policy.Full() {
		var dropped bool
		ev.Payload, _, dropped = n.policy.JSON(r.get("payload_json"))
		ev.PayloadIncluded = boolToUint8(!dropped && ev.Payload != "")
	}
	return ev
}
