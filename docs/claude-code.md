# Claude Code

Vario collects Claude Code activity from the session transcripts Claude Code keeps on your machine. Only these local files are read; Vario has no access to your Anthropic account, subscription quotas, billing or usage on other machines.

## Where the data comes from

Claude Code appends one JSON object per line to a transcript per session:

```text
<config dir>/projects/<encoded project path>/<session id>.jsonl
<config dir>/projects/<encoded project path>/<session id>/subagents/agent-<id>.jsonl   (+ agent-<id>.meta.json)
```

`<config dir>` is `$CLAUDE_CONFIG_DIR` if set, otherwise `~/.claude` (`%USERPROFILE%\.claude` on Windows). Earlier Claude Code versions wrote subagent transcripts as `agent-<id>.jsonl` next to the session, or interleaved them in the session file (`isSidechain`); all three layouts are read.

Discovery (`vario sources`, `vario init`) checks, in order, `$CLAUDE_CONFIG_DIR/projects`, `~/.claude/projects` and `~/.config/claude/projects` (`$XDG_CONFIG_HOME/claude/projects`). Override it with a path to either the configuration directory or its `projects` directory:

```bash
vario sources add claudecode --path ~/work/.claude
vario config set sources.claudecode.path /data/claude/projects
vario init --non-interactive --update --claudecode-path ~/.claude
```

`init` stores the resolved path, so the background service (which does not inherit `CLAUDE_CONFIG_DIR`) reads the same directory.

Files are opened read-only and never locked (on Windows they are opened with full sharing, so Claude Code can keep writing, renaming and deleting them).

## Enabling it

- New setups: `vario init` lists Claude Code when it is installed and offers to enable it. A non-interactive first `init` enables a detected installation (`--no-claudecode` to skip).
- Existing setups: `vario sources add claudecode`. Nothing else changes: the same ClickHouse database and schema are used, and T3 Code collection is unaffected. Restart the collector (`vario service restart`) to pick it up.
- `vario backfill --source claudecode` imports every existing transcript; `vario run` (or the service) then collects T3 Code and Claude Code together. A collector that finds no cursor for a source backfills it automatically.

`vario status` and `vario sources list` show each source's progress separately (for Claude Code: transcripts and bytes read, and lines skipped as unreadable). `vario doctor` checks the directory and reads every transcript once to report unreadable files and malformed lines (never their content).

## Historical and incremental collection

The cursor records, per transcript, the byte offset read so far (always at a line boundary), hashes of the first line and of the bytes before the offset, and the small parse state needed to continue mid-file (the open conversation, run, response and unanswered tool calls). In normal operation only appended bytes are read: an unchanged transcript costs a directory listing entry.

- **Partial lines**: a trailing line without its newline is left for the next read (unless it is already a complete JSON value).
- **Malformed lines** are skipped and counted; lines over 64 MiB are skipped.
- **Replaced, rewritten or truncated transcripts** (different first line, different bytes before the offset, or shorter than the offset) are read again from the start. Rows are keyed by Claude Code's own ids, so rows that are unchanged are not sent again.
- **Deleted transcripts** (Claude Code removes old sessions after `cleanupPeriodDays`) are dropped from the cursor; rows already delivered stay in ClickHouse.
- **Idle transcripts** (unchanged for 30 minutes) keep only their offset and hashes. If such a session is resumed, its earlier lines are re-read once, silently, to rebuild the parse state.
- Repeated backfills are idempotent; `--force` re-sends everything without changing results.

## What is collected

| Table | Claude Code data |
| --- | --- |
| `sources` | `source_kind = 'claudecode'`, host name, transcript directory, mode. `source_id` is `claudecode:<hash of machine id and directory>`. |
| `projects` | One per working directory. `project_id` is a hash of the path; title and path only in `messages`+ modes. |
| `conversations` | One per session (`conversation_id` = session id) and one per subagent (`<session id>/agent-<agent id>`). Model, permission mode (`runtime_mode`), entry point (`creation_source`), Claude Code version (attributes); title (custom title, else AI title) and git branch in `messages`+ modes. |
| `runs` | One per user prompt (`run_id` = Claude Code's prompt id), with status `running`, `completed`, `interrupted` or `incomplete`. |
| `messages` | User prompts and assistant text (one row per transcript entry, `message_id` = entry uuid). Text only in `messages`+ modes; length always. |
| `execution_events` | Tool calls with their results (`event_id` = tool use id; `Bash`/`PowerShell` → `command_execution`, `Edit`/`MultiEdit`/`Write`/`NotebookEdit` → `file_change`, `WebSearch`/`WebFetch` → `web_search`, everything else including MCP tools and `Agent` → `dynamic_tool`), reasoning blocks (`reasoning`), API errors (`error`) and context compactions (`context_compaction`). Status, error flag, timings, input/output sizes always; inputs, outputs and reasoning text only in `full` mode; edited file names in `messages`+ modes. |
| `provider_turns` | One per API response (`provider_turn_id` = Anthropic message id), with token usage. |
| `raw_events` | Every transcript line, with type, ids, timestamp and allow-listed metadata (versions, ids, model, stop reason). The line itself (`payload`) only in `full` mode. |

### Token accounting

Claude Code writes one transcript line per content block of a response, and every line repeats the response's usage (earlier versions wrote growing partial counts while streaming). Vario keeps one provider turn per Anthropic message id and the highest value of each usage field, so a response is counted once however many lines it has or however many reads it is collected over. Responses Claude Code synthesizes locally (model `<synthetic>`, API error placeholders) have no usage and produce no provider turn.

Anthropic reports `input_tokens` without cache reads and writes. Vario's columns follow its own convention:

| Vario column | From Claude Code's `usage` |
| --- | --- |
| `input_tokens` | `input_tokens + cache_creation_input_tokens + cache_read_input_tokens` |
| `cached_input_tokens` | `cache_read_input_tokens` |
| `cache_creation_input_tokens` | `cache_creation_input_tokens` |
| `uncached_input_tokens` | `input_tokens` |
| `output_tokens` | `output_tokens` (includes thinking) |
| `reasoning_tokens` | `output_tokens_details.thinking_tokens`, when reported (a subset of output) |

`provider = 'anthropic'`, `model` is the model the response reports, `usage_source = 'claudecode.message.usage'`. Service tier, speed, the 5-minute/1-hour cache-creation split, server-side web search/fetch counts, stop reason and request id are kept in `attributes`. Context-window columns stay NULL (Claude Code does not record them).

### Subagents

Each subagent transcript is its own conversation with `relationship = 'subagent'`, `parent_conversation_id` and `root_conversation_id` set to the session, and the agent id, agent type and spawning tool call (`parentToolUseId`, the `event_id` of the parent's `Agent` tool call) in `attributes`. Its responses are provider turns with `usage_scope = 'subagent'`; the session's own are `main_agent`. Claude Code records each API call once, in the transcript of the agent that made it, so within this source summing all provider turns counts each call once. Usage summaries Claude Code reports in an `Agent` tool result are not turned into provider turns. For sessions run by T3 Code, see below.

## Sessions run by T3 Code

T3 Code runs Claude through the Claude Agent SDK, so every Claude thread in T3 Code is also a Claude Code transcript on disk. Vario collects T3 Code from its own database, so collecting those transcripts here as well would count the same work twice. Instead, each piece of work is collected once, from the source that records it:

| Work | Collected from |
| --- | --- |
| Claude Code sessions (terminal, IDE, any other Agent SDK application) | Claude Code, in full |
| T3 Code threads: conversation, runs, messages, tool calls, main-agent usage | T3 Code |
| Subagents Claude starts inside a T3 Code thread: their token usage | Claude Code, as provider turns only (`usage_scope = 'subagent'`, `"harness":"t3code"` and the agent id in `attributes`, no `run_id`) |
| The same subagents' conversation, messages and tool calls | T3 Code (a child thread) |

T3 Code's database records each thread's main-agent usage (it matches the transcript's main-agent responses exactly) but not the usage of subagents, which is why only their usage is taken from the transcripts. Those provider turns have a `conversation_id` (`<session id>/agent-<agent id>`) with no conversation row in this source.

**How sessions are recognized.** A session is T3 Code's if it loaded T3 Code's MCP tools (`mcp__t3-code__*`) before its first response; every T3 Code thread does. Its lines from T3 Code's entrypoint (`t3-code`), or from the Agent SDK entrypoint (`sdk-ts`) that earlier T3 Code builds wrote, are then T3 Code's, and a subagent follows its session. The entrypoint alone does not decide: T3 Code sets it in the environment of the Claude it runs, so a `claude` that Claude itself starts from a shell command inherits `t3-code`, although T3 Code knows nothing of that session. Such sessions, and Agent SDK sessions of other applications, are collected as Claude Code. Lines without an entrypoint (titles, queue operations) follow the last line that had one. A T3 Code session later resumed in the terminal (`claude --resume`) is collected from the first terminal line on.

A new transcript is read only once the lines that decide this are written (normally within seconds: the first prompt, or for T3 Code and Agent SDK sessions the first response). One that is still undecided after 10 minutes is attributed with what it has.

**This does not depend on configuration.** Sessions run by T3 Code are attributed to T3 Code even when the T3 Code source is disabled. Neither source can retract rows it has delivered, so switching the attribution with the configuration would count the same work twice. `vario doctor` reports how many sessions were run by T3 Code and warns when T3 Code is not collected (`vario sources add t3code`).

To compare totals across both sources, sum `provider_turns` as usual: each API call is in exactly one source.

## Distinguishing sources in queries and Grafana

Both sources share the same tables and views. Filter or group by source:

```sql
SELECT day, splitByChar(':', source_id)[1] AS source, model, sum(output_tokens) AS output
FROM vario.token_usage_daily GROUP BY ALL ORDER BY day DESC;

SELECT * FROM vario.provider_turns_latest WHERE source_id LIKE 'claudecode:%';
SELECT s.source_kind, count() FROM vario.conversations_latest c
JOIN vario.sources_latest s USING (source_id) GROUP BY s.source_kind;
```

## Privacy

The collection modes apply as for every source (see [Privacy and security](privacy.md)): in `metrics` mode no prompt, reply, reasoning, tool input or output, title, branch, path or file name leaves the machine, including through raw events (which carry allow-listed metadata only) and attributes (ids, versions, counts and flags only). Redaction and `max_field_bytes` apply to every text field, and to raw payloads in `full` mode. The `session-owner` line (account and organization ids) is never copied, in any mode. A change of privacy settings re-reads all transcripts under the new settings.

Text that is collected is also kept, already redacted, in the local cursor while needed (titles, and in `full` mode the input of a tool call awaiting its result).

## Known limitations

- Only what Claude Code writes locally is available: no subscription quotas, rate limits, billing or account-wide usage, and no usage for sessions on other machines or in claude.ai / Claude Desktop. Claude Code's local cost estimates (`cost-state`) are not collected.
- Claude Code deletes old transcripts when it starts (`cleanupPeriodDays`, default 30 days); history older than that cannot be backfilled. Keep the collector running, or back up `~/.claude/projects`, to keep it.
- A T3 Code thread whose session did not load T3 Code's MCP tools would be collected from both sources. Every T3 Code thread seen so far loads them; `vario doctor` counts the sessions attributed to T3 Code.
- Lines a session copies from another session (forked or resumed sessions that start with the original's lines) are collected only from the original's transcript. If the original was already deleted, those lines are not collected as entities (they remain raw events).
- Bash exit codes are not recorded by Claude Code; failures are reported by the `is_error` flag of tool results.
- Rows of a rewritten transcript that no longer exist in it stay in ClickHouse.
- Projects carry no creation time (Claude Code has no project record).
- Large prompts copied into request-log lines (`api-request-blob`) are raw events only.
- If a future T3 Code version records its subagents' usage in its own database, those calls would be counted by both sources until Vario is updated. This was checked against T3 Code's current database, where subagent threads have no provider turns.
