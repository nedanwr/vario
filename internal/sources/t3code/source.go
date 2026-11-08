// Package t3code collects conversations, execution items and token usage from
// T3 Code's local SQLite database. The database is only ever opened read-only.
//
// Historical state is read from the projection tables. Incremental sync reads
// orchestration_events strictly as change notifications: several event types
// carry partial payloads, so the affected entities are re-read from the
// projections inside the same read transaction. The projector's high-water mark
// bounds every read, which gives each snapshot a consistent cursor boundary.
package t3code

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/nedanwr/vario/internal/model"
	"github.com/nedanwr/vario/internal/privacy"
	"github.com/nedanwr/vario/internal/sources"
)

const Kind = "t3code"

// DefaultPaths are the locations checked during discovery, in order.
func DefaultPaths() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	return []string{filepath.Join(home, ".t3", "userdata", "statev2.sqlite")}
}

type Source struct {
	path      string
	id        string
	host      string
	policy    *privacy.Policy
	batchSize int

	mu   sync.Mutex
	db   *sql.DB
	stat os.FileInfo
	// schema is cached until SQLite's schema_version changes.
	schema        schema
	schemaVersion int64
}

// New prepares a source for the database at path. The database is opened lazily.
func New(path string, policy *privacy.Policy, batchSize int) *Source {
	if batchSize <= 0 {
		batchSize = 500
	}
	host, _ := os.Hostname()
	return &Source{path: path, id: SourceID(path), host: host, policy: policy, batchSize: batchSize}
}

// SourceID derives a stable identifier from T3 Code's environment id, falling
// back to a hash of the host name and database path.
func SourceID(path string) string {
	if b, err := os.ReadFile(filepath.Join(filepath.Dir(path), "environment-id")); err == nil {
		if id := strings.TrimSpace(string(b)); id != "" && len(id) <= 128 && !strings.ContainsAny(id, " \t\r\n/") {
			return Kind + ":" + id
		}
	}
	host, _ := os.Hostname()
	sum := sha256.Sum256([]byte(host + "\x00" + path))
	return Kind + ":" + hex.EncodeToString(sum[:8])
}

func (s *Source) Kind() string     { return Kind }
func (s *Source) ID() string       { return s.id }
func (s *Source) Location() string { return s.path }

func (s *Source) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db != nil {
		err := s.db.Close()
		s.db = nil
		return err
	}
	return nil
}

// ErrUnavailable means the database cannot currently be opened; callers should retry later.
var ErrUnavailable = errors.New("t3code database unavailable")

// conn returns an open handle, reopening it if the file was replaced.
func (s *Source) conn() (*sql.DB, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := os.Stat(s.path)
	if err != nil {
		if s.db != nil {
			s.db.Close()
			s.db = nil
		}
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	if s.db != nil && s.stat != nil && os.SameFile(s.stat, st) {
		return s.db, nil
	}
	if s.db != nil {
		s.db.Close()
	}
	db, err := openReadOnly(s.path)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	s.db, s.stat, s.schema = db, st, nil
	return db, nil
}

func (s *Source) inspectCached(ctx context.Context, tx *sql.Tx) (schema, error) {
	var v int64
	if err := tx.QueryRowContext(ctx, `PRAGMA schema_version`).Scan(&v); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.schema != nil && s.schemaVersion == v {
		return s.schema, nil
	}
	sch, err := inspect(ctx, tx)
	if err != nil {
		return nil, err
	}
	s.schema, s.schemaVersion = sch, v
	return sch, nil
}

type cursor struct {
	V     int   `json:"v"`
	Epoch int64 `json:"epoch"`
	Seq   int64 `json:"seq"`
	// Anchor is the event_id at Seq, when that event exists. Event ids are
	// unique and never change, so a different id at Seq means the database was
	// replaced (for example restored from a backup) even if its log is longer.
	Anchor string `json:"anchor,omitempty"`
}

func parseCursor(s string) (cursor, bool) {
	var c cursor
	if s == "" || json.Unmarshal([]byte(s), &c) != nil || c.Epoch < 1 {
		return cursor{}, false
	}
	return c, true
}

func (c cursor) String() string {
	b, _ := json.Marshal(cursor{V: 1, Epoch: c.Epoch, Seq: c.Seq, Anchor: c.Anchor})
	return string(b)
}

// version orders record states: the epoch increments whenever the source event
// log restarts, so newer states always win even if sequence numbers repeat.
func version(epoch, seq int64) uint64 { return uint64(epoch)<<44 | uint64(seq)&(1<<44-1) }

// readTx starts a read transaction; every query inside it sees one consistent snapshot.
func (s *Source) readTx(ctx context.Context) (*sql.Tx, schema, error) {
	db, err := s.conn()
	if err != nil {
		return nil, nil, err
	}
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	sch, err := s.inspectCached(ctx, tx)
	if err != nil {
		tx.Rollback()
		return nil, nil, fmt.Errorf("%w: inspect schema: %v", ErrUnavailable, err)
	}
	if err := sch.validate(); err != nil {
		tx.Rollback()
		return nil, nil, err
	}
	return tx, sch, nil
}

func (s *Source) sourceRecord(version uint64) model.Record {
	return model.Record{Table: model.TableSources, Key: s.id, Row: &model.SourceRow{
		Envelope:       model.Envelope{SourceID: s.id, Version: version},
		SourceKind:     Kind,
		HostName:       s.host,
		Location:       s.path,
		CollectionMode: s.policy.Mode(),
	}}
}

// Snapshot emits the full projected state as of the projector's high-water mark. Raw events
// after the previous cursor (or all of them when includeHistory is set) are emitted too.
func (s *Source) Snapshot(ctx context.Context, prevCursor string, includeHistory bool, emit func([]model.Record) error) (string, error) {
	tx, sch, err := s.readTx(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	b, err := readBoundary(ctx, tx, sch)
	if err != nil {
		return "", err
	}
	cur := cursor{Epoch: 1}
	var historyAfter int64
	if prev, ok := parseCursor(prevCursor); ok {
		cur.Epoch = prev.Epoch
		reason, err := logReplaced(ctx, tx, b, prev)
		if err != nil {
			return "", err
		}
		if reason != "" {
			cur.Epoch++
		} else if !includeHistory {
			historyAfter = prev.Seq
		}
	}
	cur.Seq = b.projected
	if cur.Anchor, err = eventIDAt(ctx, tx, cur.Seq); err != nil {
		return "", err
	}
	n := &normalizer{sourceID: s.id, version: version(cur.Epoch, cur.Seq), policy: s.policy, threadCreated: map[string]model.Time{}}

	if err := emit([]model.Record{s.sourceRecord(n.version)}); err != nil {
		return "", err
	}
	if sch.has(tProjects, "project_id", "title") {
		if err := s.scan(ctx, tx, sch, specProjects, emit, func(r row) model.Record {
			return model.Record{Table: model.TableProjects, Key: r.get("project_id"), Row: ptr(n.project(r))}
		}); err != nil {
			return "", err
		}
	}
	if err := s.scan(ctx, tx, sch, specThreads, emit, func(r row) model.Record {
		c := n.conversation(r)
		n.threadCreated[c.ConversationID] = c.CreatedAt
		return model.Record{Table: model.TableConversations, Key: c.ConversationID, Row: &c}
	}); err != nil {
		return "", err
	}
	runs := map[string]runInfo{}
	if sch.has(tRuns, "run_id", "thread_id", "payload_json") {
		if err := s.scan(ctx, tx, sch, specRuns, emit, func(r row) model.Record {
			rr := n.run(r)
			runs[rr.RunID] = runInfo{runID: rr.RunID, provider: rr.Provider, providerInstanceID: rr.ProviderInstanceID, model: rr.Model}
			return model.Record{Table: model.TableRuns, Key: rr.RunID, Row: &rr}
		}); err != nil {
			return "", err
		}
	}
	attempts, err := attemptRuns(ctx, tx, sch, nil)
	if err != nil {
		return "", err
	}
	if err := s.scan(ctx, tx, sch, specMessages, emit, func(r row) model.Record {
		return model.Record{Table: model.TableMessages, Key: r.get("message_id"), Row: ptr(n.message(r))}
	}); err != nil {
		return "", err
	}
	if err := s.scan(ctx, tx, sch, specItems, emit, func(r row) model.Record {
		return model.Record{Table: model.TableExecutionEvents, Key: r.get("turn_item_id"), Row: ptr(n.executionEvent(r))}
	}); err != nil {
		return "", err
	}
	if err := s.scan(ctx, tx, sch, specProviderTurns, emit, func(r row) model.Record {
		return model.Record{Table: model.TableProviderTurns, Key: r.get("provider_turn_id"),
			Row: ptr(n.providerTurn(r, runs[attempts[r.get("run_attempt_id")]]))}
	}); err != nil {
		return "", err
	}
	if includeHistory || historyAfter < cur.Seq {
		after := historyAfter
		for {
			evs, err := readEvents(ctx, tx, sch, after, cur.Seq, s.batchSize)
			if err != nil {
				return "", err
			}
			if len(evs) == 0 {
				break
			}
			recs := make([]model.Record, len(evs))
			for i, e := range evs {
				recs[i] = rawRecord(n, e)
			}
			if err := emit(recs); err != nil {
				return "", err
			}
			after = recs[len(recs)-1].Row.(*model.RawEventRow).Sequence
		}
	}
	return cur.String(), nil
}

func ptr[T any](v T) *T { return &v }

func rawRecord(n *normalizer, e row) model.Record {
	ev := n.rawEvent(e)
	return model.Record{Table: model.TableRawEvents, Key: strconv.FormatInt(ev.Sequence, 10) + "/" + ev.EventID, Row: &ev}
}

func (s *Source) scan(ctx context.Context, q querier, sch schema, spec tableSpec, emit func([]model.Record) error, conv func(row) model.Record) error {
	after := ""
	for {
		rows, err := spec.page(ctx, q, sch, after, s.batchSize)
		if err != nil {
			return fmt.Errorf("read %s: %w", spec.table, err)
		}
		if len(rows) == 0 {
			return nil
		}
		recs := make([]model.Record, 0, len(rows))
		for _, r := range rows {
			recs = append(recs, conv(r))
		}
		if err := emit(recs); err != nil {
			return err
		}
		after = rows[len(rows)-1].get(spec.id)
		if len(rows) < s.batchSize {
			return nil
		}
	}
}

// attemptRuns maps run attempt ids to run ids. A nil ids slice loads every attempt.
func attemptRuns(ctx context.Context, q querier, sch schema, ids []string) (map[string]string, error) {
	out := map[string]string{}
	if !sch.has(tRunAttempts, "attempt_id", "run_id") {
		return out, nil
	}
	query := fmt.Sprintf(`SELECT attempt_id, run_id FROM "%s"`, tRunAttempts)
	var args []any
	if ids != nil {
		if len(ids) == 0 {
			return out, nil
		}
		query += ` WHERE attempt_id IN (` + strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",") + `)`
		for _, id := range ids {
			args = append(args, id)
		}
	}
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var a, r string
		if err := rows.Scan(&a, &r); err != nil {
			return nil, err
		}
		out[a] = r
	}
	return out, rows.Err()
}

// Changes reads events after the cursor and re-reads the entities they touched.
func (s *Source) Changes(ctx context.Context, cursorStr string, limit int) (sources.ChangeSet, error) {
	cur, ok := parseCursor(cursorStr)
	if !ok {
		return sources.ChangeSet{Reconcile: "no valid cursor"}, nil
	}
	tx, sch, err := s.readTx(ctx)
	if err != nil {
		return sources.ChangeSet{}, err
	}
	defer tx.Rollback()
	b, err := readBoundary(ctx, tx, sch)
	if err != nil {
		return sources.ChangeSet{}, err
	}
	reason, err := logReplaced(ctx, tx, b, cur)
	if err != nil {
		return sources.ChangeSet{}, err
	}
	switch {
	case reason != "":
		return sources.ChangeSet{Reconcile: reason}, nil
	case b.pruneMark > cur.Seq && b.pruneMark <= b.projected:
		return sources.ChangeSet{Reconcile: "T3 Code compacted events after the stored cursor"}, nil
	case b.projected <= cur.Seq:
		return sources.ChangeSet{Cursor: cursorStr}, nil
	}

	evs, err := readEvents(ctx, tx, sch, cur.Seq, b.projected, limit)
	if err != nil {
		return sources.ChangeSet{}, err
	}
	n := &normalizer{sourceID: s.id, version: version(cur.Epoch, b.projected), policy: s.policy, threadCreated: map[string]model.Time{}}
	touched := map[string]map[string]bool{}
	touch := func(kind, id string) {
		if id == "" {
			return
		}
		if touched[kind] == nil {
			touched[kind] = map[string]bool{}
		}
		touched[kind][id] = true
	}
	var recs []model.Record
	for _, e := range evs {
		recs = append(recs, rawRecord(n, e))
		et := e.get("event_type")
		id := parsePayload(e.get("payload_json")).str("id")
		switch {
		case et == "message.updated":
			touch(tMessages, id)
		case et == "turn-item.updated":
			touch(tTurnItems, id)
		case et == "provider-turn.updated":
			touch(tProviderTurns, id)
		case et == "run.created" || et == "run.updated":
			touch(tRuns, id)
		case strings.HasPrefix(et, "thread."):
			touch(tThreads, e.get("stream_id"))
		case strings.HasPrefix(et, "project."):
			touch(tProjects, first(parsePayload(e.get("payload_json")).str("projectId"), e.get("stream_id")))
		}
	}

	load := func(spec tableSpec, kind string) ([]row, error) {
		if len(touched[kind]) == 0 || !sch.has(spec.table, spec.id) {
			return nil, nil
		}
		return spec.byIDs(ctx, tx, sch, keys(touched[kind]))
	}
	threadRows, err := load(specThreads, tThreads)
	if err != nil {
		return sources.ChangeSet{}, err
	}
	messages, err := load(specMessages, tMessages)
	if err != nil {
		return sources.ChangeSet{}, err
	}
	items, err := load(specItems, tTurnItems)
	if err != nil {
		return sources.ChangeSet{}, err
	}
	turns, err := load(specProviderTurns, tProviderTurns)
	if err != nil {
		return sources.ChangeSet{}, err
	}
	runRows, err := load(specRuns, tRuns)
	if err != nil {
		return sources.ChangeSet{}, err
	}
	projects, err := load(specProjects, tProjects)
	if err != nil {
		return sources.ChangeSet{}, err
	}

	// Resolve immutable conversation creation times for every affected thread.
	threadIDs := map[string]bool{}
	for _, set := range [][]row{messages, items, turns, runRows} {
		for _, r := range set {
			threadIDs[r.get("thread_id")] = true
		}
	}
	for _, r := range threadRows {
		delete(threadIDs, r.get("thread_id"))
		n.threadCreated[r.get("thread_id")] = model.ParseTime(first(r.get("created_at"), parsePayload(r.get("payload_json")).str("createdAt")))
	}
	if len(threadIDs) > 0 {
		extra, err := specThreads.byIDs(ctx, tx, sch, keys(threadIDs))
		if err != nil {
			return sources.ChangeSet{}, err
		}
		for _, r := range extra {
			n.threadCreated[r.get("thread_id")] = model.ParseTime(first(r.get("created_at"), parsePayload(r.get("payload_json")).str("createdAt")))
		}
	}

	// Resolve the run behind each provider turn for provider and model attribution.
	attemptIDs := []string{}
	for _, r := range turns {
		if a := r.get("run_attempt_id"); a != "" {
			attemptIDs = append(attemptIDs, a)
		}
	}
	attempts, err := attemptRuns(ctx, tx, sch, attemptIDs)
	if err != nil {
		return sources.ChangeSet{}, err
	}
	runs := map[string]runInfo{}
	runIDs := map[string]bool{}
	for _, r := range attempts {
		runIDs[r] = true
	}
	for _, r := range runRows {
		delete(runIDs, r.get("run_id"))
	}
	runSource := runRows
	if len(runIDs) > 0 && sch.has(tRuns, "run_id") {
		extra, err := specRuns.byIDs(ctx, tx, sch, keys(runIDs))
		if err != nil {
			return sources.ChangeSet{}, err
		}
		runSource = append(append([]row{}, runRows...), extra...)
	}
	for _, r := range runSource {
		rr := n.run(r)
		runs[rr.RunID] = runInfo{runID: rr.RunID, provider: rr.Provider, providerInstanceID: rr.ProviderInstanceID, model: rr.Model}
	}

	for _, r := range projects {
		recs = append(recs, model.Record{Table: model.TableProjects, Key: r.get("project_id"), Row: ptr(n.project(r))})
	}
	for _, r := range threadRows {
		recs = append(recs, model.Record{Table: model.TableConversations, Key: r.get("thread_id"), Row: ptr(n.conversation(r))})
	}
	for _, r := range runRows {
		recs = append(recs, model.Record{Table: model.TableRuns, Key: r.get("run_id"), Row: ptr(n.run(r))})
	}
	for _, r := range messages {
		recs = append(recs, model.Record{Table: model.TableMessages, Key: r.get("message_id"), Row: ptr(n.message(r))})
	}
	for _, r := range items {
		recs = append(recs, model.Record{Table: model.TableExecutionEvents, Key: r.get("turn_item_id"), Row: ptr(n.executionEvent(r))})
	}
	for _, r := range turns {
		recs = append(recs, model.Record{Table: model.TableProviderTurns, Key: r.get("provider_turn_id"),
			Row: ptr(n.providerTurn(r, runs[attempts[r.get("run_attempt_id")]]))})
	}

	next := cursor{Epoch: cur.Epoch, Seq: b.projected}
	more := len(evs) == limit
	if more {
		next.Seq = n.rawSeq(evs[len(evs)-1])
		next.Anchor = evs[len(evs)-1].get("event_id")
	} else if next.Anchor, err = eventIDAt(ctx, tx, next.Seq); err != nil {
		return sources.ChangeSet{}, err
	}
	return sources.ChangeSet{Records: recs, Cursor: next.String(), More: more}, nil
}

func (n *normalizer) rawSeq(e row) int64 {
	v, _ := strconv.ParseInt(e.get("sequence"), 10, 64)
	return v
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Detect inspects candidate database paths without modifying them.
func Detect(ctx context.Context, paths []string) []sources.Detection {
	var out []sources.Detection
	for _, p := range paths {
		out = append(out, detectOne(ctx, p))
	}
	return out
}

func detectOne(ctx context.Context, path string) sources.Detection {
	d := sources.Detection{Kind: Kind, Path: path}
	st, err := os.Stat(path)
	if err != nil {
		d.Detail = "not found"
		if !errors.Is(err, os.ErrNotExist) {
			d.Detail = err.Error()
		}
		return d
	}
	d.Exists = true
	d.Facts = append(d.Facts, sources.Fact{Name: "size", Value: humanBytes(st.Size())})
	db, err := openReadOnly(path)
	if err != nil {
		d.Detail = "cannot open read-only: " + err.Error()
		return d
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		d.Detail = "cannot read: " + err.Error()
		return d
	}
	defer tx.Rollback()
	sch, err := inspect(ctx, tx)
	if err != nil {
		d.Detail = "cannot read schema: " + err.Error()
		return d
	}
	if err := sch.validate(); err != nil {
		d.Detail = err.Error()
		return d
	}
	d.Compatible = true
	d.Detail = "compatible"
	count := func(table string) string {
		var c int64
		if err := tx.QueryRowContext(ctx, fmt.Sprintf(`SELECT COUNT(*) FROM "%s"`, table)).Scan(&c); err != nil {
			return "?"
		}
		return strconv.FormatInt(c, 10)
	}
	d.Facts = append(d.Facts,
		sources.Fact{Name: "conversations", Value: count(tThreads)},
		sources.Fact{Name: "messages", Value: count(tMessages)},
		sources.Fact{Name: "execution items", Value: count(tTurnItems)},
		sources.Fact{Name: "provider turns", Value: count(tProviderTurns)},
	)
	if b, err := readBoundary(ctx, tx, sch); err == nil {
		d.Facts = append(d.Facts, sources.Fact{Name: "event sequence", Value: fmt.Sprintf("%d (projected %d)", b.maxSequence, b.projected)})
	}
	var last sql.NullString
	if tx.QueryRowContext(ctx, fmt.Sprintf(`SELECT occurred_at FROM "%s" ORDER BY sequence DESC LIMIT 1`, tEvents)).Scan(&last) == nil && last.Valid {
		d.Facts = append(d.Facts, sources.Fact{Name: "last activity", Value: last.String})
	}
	d.Facts = append(d.Facts, sources.Fact{Name: "source id", Value: SourceID(path)})
	return d
}

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

// logReplaced reports why the event log no longer continues from c, or "".
func logReplaced(ctx context.Context, q querier, b boundary, c cursor) (string, error) {
	if b.maxSequence < c.Seq {
		return "the T3 Code event log restarted (database replaced or reset)", nil
	}
	if c.Anchor == "" {
		return "", nil
	}
	id, err := eventIDAt(ctx, q, c.Seq)
	if err != nil {
		return "", err
	}
	// A missing event was pruned; that is not evidence of replacement.
	if id != "" && id != c.Anchor {
		return "the T3 Code database was replaced (event at the stored cursor differs)", nil
	}
	return "", nil
}

// eventIDAt returns the id of the event with the given sequence, or "" if there is none.
func eventIDAt(ctx context.Context, q querier, seq int64) (string, error) {
	if seq <= 0 {
		return "", nil
	}
	var id string
	err := q.QueryRowContext(ctx, fmt.Sprintf(`SELECT event_id FROM "%s" WHERE sequence = ?`, tEvents), seq).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return id, err
}
