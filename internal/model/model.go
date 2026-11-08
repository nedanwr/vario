// Package model defines the normalized records Vario delivers to storage.
//
// Each row type maps one-to-one onto a ClickHouse table; JSON field names are
// column names. Mutable entities carry a Version so that the latest state wins
// deterministically under ReplacingMergeTree.
package model

import (
	"bytes"
	"encoding/json"
	"time"
)

const (
	TableSources         = "sources"
	TableProjects        = "projects"
	TableConversations   = "conversations"
	TableRuns            = "runs"
	TableMessages        = "messages"
	TableExecutionEvents = "execution_events"
	TableProviderTurns   = "provider_turns"
	TableRawEvents       = "raw_events"
)

// Tables lists every destination table in dependency-free delivery order.
var Tables = []string{
	TableSources, TableProjects, TableConversations, TableRuns,
	TableMessages, TableExecutionEvents, TableProviderTurns, TableRawEvents,
}

// Record is one normalized row bound for a table. Key identifies the logical
// entity within its source and is stable across retries and updates.
type Record struct {
	Table string
	Key   string
	Row   Row
}

type Row interface{ Env() *Envelope }

type Envelope struct {
	SourceID string `json:"source_id"`
	Version  uint64 `json:"version"`
}

func (e *Envelope) Env() *Envelope { return e }

// Time serializes as a ClickHouse DateTime64(3) literal in UTC, or null when zero.
type Time struct{ time.Time }

const chTimeLayout = "2006-01-02 15:04:05.000"

func (t Time) MarshalJSON() ([]byte, error) {
	if t.IsZero() {
		return []byte("null"), nil
	}
	return []byte(`"` + t.UTC().Format(chTimeLayout) + `"`), nil
}

func (t *Time) UnmarshalJSON(b []byte) error {
	if bytes.Equal(b, []byte("null")) {
		t.Time = time.Time{}
		return nil
	}
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	v, err := time.Parse(chTimeLayout, s)
	if err != nil {
		return err
	}
	t.Time = v.UTC()
	return nil
}

// ParseTime parses the RFC 3339 timestamps used by sources; invalid or empty input yields zero.
func ParseTime(s string) Time {
	if s == "" {
		return Time{}
	}
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02 15:04:05.999999999", "2006-01-02T15:04:05.999999999"} {
		if v, err := time.Parse(layout, s); err == nil {
			return Time{v.UTC()}
		}
	}
	return Time{}
}

type SourceRow struct {
	Envelope
	SourceKind     string `json:"source_kind"`
	HostName       string `json:"host_name"`
	Location       string `json:"location"`
	CollectionMode string `json:"collection_mode"`
}

type ProjectRow struct {
	Envelope
	ProjectID     string `json:"project_id"`
	Title         string `json:"title"`
	WorkspaceRoot string `json:"workspace_root"`
	CreatedAt     Time   `json:"created_at"`
	UpdatedAt     Time   `json:"updated_at"`
	DeletedAt     Time   `json:"deleted_at"`
	IsDeleted     uint8  `json:"is_deleted"`
}

type ConversationRow struct {
	Envelope
	ConversationID       string `json:"conversation_id"`
	ProjectID            string `json:"project_id"`
	Title                string `json:"title"`
	Provider             string `json:"provider"`
	ProviderInstanceID   string `json:"provider_instance_id"`
	Model                string `json:"model"`
	RuntimeMode          string `json:"runtime_mode"`
	InteractionMode      string `json:"interaction_mode"`
	Branch               string `json:"branch"`
	ParentConversationID string `json:"parent_conversation_id"`
	RootConversationID   string `json:"root_conversation_id"`
	Relationship         string `json:"relationship"`
	CreatedBy            string `json:"created_by"`
	CreationSource       string `json:"creation_source"`
	CreatedAt            Time   `json:"created_at"`
	UpdatedAt            Time   `json:"updated_at"`
	ArchivedAt           Time   `json:"archived_at"`
	DeletedAt            Time   `json:"deleted_at"`
	IsDeleted            uint8  `json:"is_deleted"`
	Attributes           string `json:"attributes"`
}

// Scoped is embedded by rows that belong to a conversation. ConversationCreatedAt
// is immutable and drives partitioning so that every version of a row lands in
// the same partition and deduplicates correctly.
type Scoped struct {
	Envelope
	ConversationID        string `json:"conversation_id"`
	ConversationCreatedAt Time   `json:"conversation_created_at"`
}

type RunRow struct {
	Scoped
	RunID              string `json:"run_id"`
	Ordinal            int64  `json:"ordinal"`
	Provider           string `json:"provider"`
	ProviderInstanceID string `json:"provider_instance_id"`
	Model              string `json:"model"`
	ModelOptions       string `json:"model_options"`
	Status             string `json:"status"`
	UserMessageID      string `json:"user_message_id"`
	RequestedAt        Time   `json:"requested_at"`
	StartedAt          Time   `json:"started_at"`
	CompletedAt        Time   `json:"completed_at"`
	Attributes         string `json:"attributes"`
}

type MessageRow struct {
	Scoped
	MessageID       string `json:"message_id"`
	RunID           string `json:"run_id"`
	NodeID          string `json:"node_id"`
	Role            string `json:"role"`
	CreatedBy       string `json:"created_by"`
	CreationSource  string `json:"creation_source"`
	Streaming       uint8  `json:"streaming"`
	Text            string `json:"text"`
	TextChars       uint64 `json:"text_chars"`
	ContentIncluded uint8  `json:"content_included"`
	AttachmentCount uint32 `json:"attachment_count"`
	Attachments     string `json:"attachments"`
	Redactions      uint32 `json:"redactions"`
	Truncated       uint8  `json:"truncated"`
	CreatedAt       Time   `json:"created_at"`
	UpdatedAt       Time   `json:"updated_at"`
}

type ExecutionEventRow struct {
	Scoped
	EventID          string `json:"event_id"`
	RunID            string `json:"run_id"`
	NodeID           string `json:"node_id"`
	ProviderThreadID string `json:"provider_thread_id"`
	ProviderTurnID   string `json:"provider_turn_id"`
	ParentEventID    string `json:"parent_event_id"`
	Ordinal          int64  `json:"ordinal"`
	EventType        string `json:"event_type"`
	Status           string `json:"status"`
	IsError          uint8  `json:"is_error"`
	ToolName         string `json:"tool_name"`
	MessageID        string `json:"message_id"`
	ExitCode         *int64 `json:"exit_code"`
	Title            string `json:"title"`
	Input            string `json:"input"`
	Output           string `json:"output"`
	InputBytes       uint64 `json:"input_bytes"`
	OutputBytes      uint64 `json:"output_bytes"`
	FileName         string `json:"file_name"`
	ContentIncluded  uint8  `json:"content_included"`
	Redactions       uint32 `json:"redactions"`
	Truncated        uint8  `json:"truncated"`
	StartedAt        Time   `json:"started_at"`
	CompletedAt      Time   `json:"completed_at"`
	UpdatedAt        Time   `json:"updated_at"`
	DurationMs       *int64 `json:"duration_ms"`
	Attributes       string `json:"attributes"`
}

// ProviderTurnRow carries two distinct, separately reported measurements:
//
//   - Turn usage (turnTokenUsage): tokens consumed by the provider turn.
//     input_tokens already includes cached and cache-creation tokens, and
//     output_tokens already includes reasoning tokens where the provider reports both.
//   - Context snapshot (tokenUsage): the context window occupancy at the last update.
//
// Missing measurements are null, never zero.
type ProviderTurnRow struct {
	Scoped
	ProviderTurnID     string   `json:"provider_turn_id"`
	ProviderThreadID   string   `json:"provider_thread_id"`
	RunID              string   `json:"run_id"`
	RunAttemptID       string   `json:"run_attempt_id"`
	NodeID             string   `json:"node_id"`
	Ordinal            int64    `json:"ordinal"`
	Status             string   `json:"status"`
	Provider           string   `json:"provider"`
	ProviderInstanceID string   `json:"provider_instance_id"`
	Model              string   `json:"model"`
	ModelSource        string   `json:"model_source"`
	Models             []string `json:"models"`
	StartedAt          Time     `json:"started_at"`
	CompletedAt        Time     `json:"completed_at"`
	DurationMs         *int64   `json:"duration_ms"`

	UsageSource              string  `json:"usage_source"`
	UsageScope               string  `json:"usage_scope"`
	UsageStatus              string  `json:"usage_status"`
	HasSubagents             *uint8  `json:"has_subagents"`
	InputTokens              *uint64 `json:"input_tokens"`
	CachedInputTokens        *uint64 `json:"cached_input_tokens"`
	CacheCreationInputTokens *uint64 `json:"cache_creation_input_tokens"`
	UncachedInputTokens      *uint64 `json:"uncached_input_tokens"`
	OutputTokens             *uint64 `json:"output_tokens"`
	ReasoningTokens          *uint64 `json:"reasoning_tokens"`
	OtherInputTokens         *uint64 `json:"other_input_tokens"`
	OtherOutputTokens        *uint64 `json:"other_output_tokens"`

	ContextUsedTokens            *uint64 `json:"context_used_tokens"`
	ContextMaxTokens             *uint64 `json:"context_max_tokens"`
	ContextInputTokens           *uint64 `json:"context_input_tokens"`
	ContextCachedInputTokens     *uint64 `json:"context_cached_input_tokens"`
	ContextOutputTokens          *uint64 `json:"context_output_tokens"`
	ContextReasoningOutputTokens *uint64 `json:"context_reasoning_output_tokens"`
	ContextUpdatedAt             Time    `json:"context_updated_at"`

	Attributes string `json:"attributes"`
}

// RawEventRow is an immutable source event. Payload is only populated in full mode.
type RawEventRow struct {
	Envelope
	Sequence        int64  `json:"sequence"`
	EventID         string `json:"event_id"`
	AggregateKind   string `json:"aggregate_kind"`
	StreamID        string `json:"stream_id"`
	EventType       string `json:"event_type"`
	OccurredAt      Time   `json:"occurred_at"`
	Metadata        string `json:"metadata"`
	Payload         string `json:"payload"`
	PayloadIncluded uint8  `json:"payload_included"`
}
