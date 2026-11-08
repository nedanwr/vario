# Vario

**Observe everything AI.**

Vario is a lightweight, cross-platform command-line tool for AI observability. It collects, archives, and analyzes your AI interactions: conversations, token usage, tool executions, and other execution telemetry.

> **Status:** Vario is in its initial development stage. There is no usable release yet, and interfaces may change without notice.

## Core capabilities (planned)

- **Collect:** ingest interaction data from local AI coding applications, starting with T3 Code, Codex, and Claude Code, with more sources to follow.
- **Archive:** keep a durable, queryable history of conversations and sessions.
- **Analyze:** report on token usage, tool executions, and activity across tools and projects.

## Technology

- **Go:** a single, portable CLI binary.
- **ClickHouse:** the primary analytical storage backend.

## Principles

Vario is open-source and local-first. Your data stays under your control. Vario is designed to work without hosted accounts or external telemetry services.

## License

Vario is licensed under the [Apache License 2.0](LICENSE).
