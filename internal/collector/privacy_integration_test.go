package collector

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nedanwr/vario/internal/config"
	"github.com/nedanwr/vario/internal/model"
	"github.com/nedanwr/vario/internal/privacy"
	"github.com/nedanwr/vario/internal/sources"
	"github.com/nedanwr/vario/internal/sources/t3code"
	"github.com/nedanwr/vario/internal/spool"
	"github.com/nedanwr/vario/internal/storage/clickhouse"
)

// F1: privacy-policy versioning. A row queued under earlier privacy settings
// (a collection mode or redaction rules) that is retried after the record
// was reprocessed under newer settings must never become the record's latest
// version in ClickHouse, whatever the delivery order, retries, restarts or
// destination switches. Every test checks the *_latest views of a real
// ClickHouse database, never only that inserts succeeded. All content is
// synthetic: markers starting with "SYNTH-" stand for sensitive text.

// gateSink is the operational client with injected delivery faults. It
// records every insert that reached ClickHouse, in order.
type gateSink struct {
	*clickhouse.Client
	mu sync.Mutex
	// mode: "" passes everything through; "fail" refuses inserts without
	// sending them; "lost" sends inserts and then reports a lost response;
	// "down" fails every call, like an unreachable server.
	mode    string
	inserts []recordedInsert
}

var errInjectedDown = &clickhouse.Error{Message: "dial tcp: connection refused", Transient: true}

func (s *gateSink) set(mode string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.mode = mode
}

func (s *gateSink) current() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.mode
}

func (s *gateSink) Insert(ctx context.Context, table string, body []byte, token string) error {
	switch s.current() {
	case "down":
		return errInjectedDown
	case "fail":
		return &clickhouse.Error{Status: 503, Message: "injected insert failure", Transient: true}
	}
	if err := s.Client.Insert(ctx, table, body, token); err != nil {
		return err
	}
	s.mu.Lock()
	s.inserts = append(s.inserts, recordedInsert{table, token, append([]byte(nil), body...)})
	lost := s.mode == "lost"
	s.mu.Unlock()
	if lost {
		return errInjectedLost
	}
	return nil
}

func (s *gateSink) CheckDestination(ctx context.Context) (clickhouse.DestinationInfo, error) {
	if s.current() == "down" {
		return clickhouse.DestinationInfo{}, errInjectedDown
	}
	return s.Client.CheckDestination(ctx)
}

func (s *gateSink) ReadCheckpoint(ctx context.Context, writer string) (uint64, bool, error) {
	if s.current() == "down" {
		return 0, false, errInjectedDown
	}
	return s.Client.ReadCheckpoint(ctx, writer)
}

func (s *gateSink) WriteCheckpoint(ctx context.Context, writer string, seq uint64) error {
	if s.current() == "down" {
		return errInjectedDown
	}
	return s.Client.WriteCheckpoint(ctx, writer, seq)
}

func (s *gateSink) MaxVersion(ctx context.Context, ids []string) (uint64, error) {
	if s.current() == "down" {
		return 0, errInjectedDown
	}
	return s.Client.MaxVersion(ctx, ids)
}

func (s *gateSink) recorded() []recordedInsert {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]recordedInsert(nil), s.inserts...)
}

func gunzip(t *testing.T, b []byte) string {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	out, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

// interruptSource fails a snapshot after `after` emitted batches, like a
// collector that is killed during a re-read.
type interruptSource struct {
	*t3code.Source
	after int
}

var errInterrupted = errors.New("injected interruption")

func (s *interruptSource) Snapshot(ctx context.Context, prev string, history bool, emit func([]model.Record) error) (string, error) {
	n := 0
	return s.Source.Snapshot(ctx, prev, history, func(recs []model.Record) error {
		if n >= s.after {
			return errInterrupted
		}
		n++
		return emit(recs)
	})
}

// privEnv is a recoveryEnv with one database, a gateSink and switchable
// privacy settings.
type privEnv struct {
	*recoveryEnv
	db   string
	sink *gateSink
	mode string
}

type policySpec struct {
	mode     string
	redact   bool
	patterns []string
}

func spec(mode string, patterns ...string) policySpec {
	return policySpec{mode: mode, redact: true, patterns: patterns}
}

func newPrivEnv(t *testing.T, p policySpec) *privEnv {
	t.Helper()
	e := &privEnv{recoveryEnv: newRecoveryEnv(t)}
	e.db = fmt.Sprintf("vario_f1_%d", time.Now().UnixNano())
	e.migrate(e.db)
	e.sink = &gateSink{Client: e.clients[e.db]}
	e.setPolicy(p)
	return e
}

// setPolicy changes the privacy settings, as editing the configuration and
// restarting the collector does.
func (e *privEnv) setPolicy(p policySpec) {
	e.t.Helper()
	pc := config.Default().Privacy
	pc.Mode, pc.RedactSecrets, pc.ExtraPatterns = p.mode, p.redact, p.patterns
	pc.AllowSensitiveContent = p.mode != config.ModeMetrics
	pol, err := privacy.New(pc)
	if err != nil {
		e.t.Fatal(err)
	}
	e.src.Close()
	e.pol, e.mode = pol, p.mode
	e.src = t3code.New(e.fix.Path, pol, 2)
	e.t.Cleanup(func() { e.src.Close() })
}

func (e *privEnv) opts(db string) Options {
	return Options{PollInterval: 20 * time.Millisecond, FlushInterval: 20 * time.Millisecond, BatchSize: 2, MaxInsertRows: 3,
		// A failed entry is not due again for an hour: deliveries at "now"
		// only send entries that were never attempted (deliverFresh), and the
		// stale retries arrive when the test lets them (deliverAll).
		RetryInitial: time.Hour, RetryMax: time.Hour,
		PolicyFingerprint: e.pol.Fingerprint(), Endpoint: e.endpoint, Database: db}
}

// collectorFor is a freshly started collector for db (as after a restart)
// with src as its only source, delivering through sink.
func (e *privEnv) collectorFor(db string, sink Sink, src sources.Source) *Collector {
	e.t.Helper()
	c := New(e.store, sink, []sources.Source{src}, e.opts(db), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if _, err := c.Prepare(ctx, true); err != nil {
		e.t.Fatal(err)
	}
	return c
}

func (e *privEnv) collector() *Collector { return e.collectorFor(e.db, e.sink, e.src) }

// sync runs one source sync of a running collector.
func (e *privEnv) sync(c *Collector) {
	e.t.Helper()
	if err := c.syncOnce(ctx, c.sources[0], c.log); err != nil {
		e.t.Fatal(err)
	}
}

func (e *privEnv) stats() spool.Stats {
	e.t.Helper()
	st, err := e.store.Stats(ctx)
	if err != nil {
		e.t.Fatal(err)
	}
	return st
}

// queueStale attempts every queued entry once with the sink in mode ("fail":
// the rows never arrive; "lost": they arrive but the response is lost). The
// entries stay queued with their next attempt an hour away.
func (e *privEnv) queueStale(c *Collector, mode string) {
	e.t.Helper()
	if e.stats().Entries == 0 {
		e.t.Fatal("queueStale: nothing queued")
	}
	e.sink.set(mode)
	defer e.sink.set("")
	now := time.Now()
	for i := 0; e.stats().FailingEntries < e.stats().Entries; i++ {
		if i > 200 {
			e.t.Fatalf("queueStale: entries not attempted: %+v", e.stats())
		}
		if _, err := c.deliverRound(ctx, now); err == nil || isFatal(err) {
			e.t.Fatalf("queueStale(%s): expected a delivery failure, got %v", mode, err)
		}
	}
}

// deliverFresh delivers the entries that were never attempted; stale
// entries are not due yet.
func (e *privEnv) deliverFresh(c *Collector) {
	e.t.Helper()
	for i := 0; ; i++ {
		n, err := c.deliverRound(ctx, time.Now())
		if err != nil {
			e.t.Fatal(err)
		}
		if n == 0 {
			return
		}
		if i > 500 {
			e.t.Fatal("deliverFresh did not finish")
		}
	}
}

// deliverAll delivers everything, the stale retries included (under new
// tokens), until the spool is empty.
func (e *privEnv) deliverAll(c *Collector) {
	e.t.Helper()
	for i := 0; e.stats().Entries > 0; i++ {
		if _, err := c.deliverRound(ctx, time.Now().Add(48*time.Hour)); err != nil {
			e.t.Fatal(err)
		}
		if i > 500 {
			e.t.Fatalf("deliverAll did not drain: %+v", e.stats())
		}
	}
}

// rows returns every row of a view or table of db; version is also returned
// exactly, as version_s.
func (e *privEnv) rows(db, name, where string) []map[string]any {
	e.t.Helper()
	var out []map[string]any
	q := fmt.Sprintf("SELECT *, toString(version) AS version_s FROM `%s`.`%s`", db, name)
	if where != "" {
		q += " WHERE " + where
	}
	if err := e.clients[db].Query(ctx, q, &out); err != nil {
		e.t.Fatal(err)
	}
	return out
}

// leaks lists the latest rows (over every *_latest view of db) containing marker.
func (e *privEnv) leaks(db, marker string) []string {
	e.t.Helper()
	var out []string
	for _, table := range model.Tables {
		for _, r := range e.rows(db, table+"_latest", "") {
			b, _ := json.Marshal(r)
			if strings.Contains(string(b), marker) {
				out = append(out, fmt.Sprintf("%s_latest: %s", table, b))
			}
		}
	}
	return out
}

func (e *privEnv) requireNoLeak(db, marker, when string) {
	e.t.Helper()
	if l := e.leaks(db, marker); len(l) > 0 {
		e.t.Fatalf("%s: %d latest rows expose %q, e.g. %s", when, len(l), marker, l[0])
	}
}

// physical counts the rows of a base table (every stored version) containing marker.
func (e *privEnv) physical(db, table, marker string) int {
	n := 0
	for _, r := range e.rows(db, table, "") {
		b, _ := json.Marshal(r)
		if strings.Contains(string(b), marker) {
			n++
		}
	}
	return n
}

// latest returns the latest row of one record.
func (e *privEnv) latest(db, table, keyCol, key string) map[string]any {
	e.t.Helper()
	rs := e.rows(db, table+"_latest", fmt.Sprintf("%s = '%s'", keyCol, key))
	if len(rs) != 1 {
		e.t.Fatalf("%s_latest %s=%s: %d rows", table, keyCol, key, len(rs))
	}
	return rs[0]
}

func generationOf(t *testing.T, r map[string]any) uint64 {
	t.Helper()
	v, err := strconv.ParseUint(r["version_s"].(string), 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	return model.Generation(v)
}

// sensitive adds a conversation whose title, message text, command input
// and output and raw event payloads carry synthetic sensitive markers, with
// a provider turn of 1000 input tokens.
func (e *privEnv) sensitive(n int) {
	thread, run := fmt.Sprintf("thread-s%d", n), fmt.Sprintf("run-s%d", n)
	e.fix.Thread(thread, "proj-1", fmt.Sprintf("SYNTH-TITLE-%d confidential plan", n), "2026-03-02T09:00:00.000Z")
	e.fix.Run(run, thread, 1, "claude-test-1")
	e.fix.Message(fmt.Sprintf("msg-s%d", n), thread, run, "user", fmt.Sprintf("SYNTH-MSG-%d my diagnosis is private", n), false)
	e.fix.Item(fmt.Sprintf("item-s%d", n), thread, "command_execution", "completed", 1,
		map[string]any{"input": fmt.Sprintf("cat SYNTH-EXEC-%d.env", n), "output": fmt.Sprintf("SYNTH-EXEC-%d=classified", n), "exitCode": 0})
	e.fix.ProviderTurn(fmt.Sprintf("turn-s%d", n), thread, run+":attempt:1", "completed", nil,
		map[string]any{"usageScope": "main_agent", "usageStatus": "complete", "inputTokens": 1000, "outputTokens": 10}, nil)
}

// requireMode verifies that the latest rows of db expose exactly what mode
// allows of the sensitive conversations 1..n.
func (e *privEnv) requireMode(db, mode string, n int, when string) {
	e.t.Helper()
	switch mode {
	case config.ModeMetrics:
		e.requireNoLeak(db, "SYNTH-", when)
	case config.ModeMessages:
		e.requireNoLeak(db, "SYNTH-EXEC", when)
		for _, l := range e.leaks(db, "SYNTH-MSG") {
			if !strings.HasPrefix(l, "messages_latest") {
				e.t.Fatalf("%s: message text outside messages_latest in messages mode: %s", when, l)
			}
		}
	}
	for i := 1; i <= n; i++ {
		msg := e.latest(db, model.TableMessages, "message_id", fmt.Sprintf("msg-s%d", i))
		ev := e.latest(db, model.TableExecutionEvents, "event_id", fmt.Sprintf("item-s%d", i))
		conv := e.latest(db, model.TableConversations, "conversation_id", fmt.Sprintf("thread-s%d", i))
		hasText := strings.Contains(fmt.Sprint(msg["text"]), fmt.Sprintf("SYNTH-MSG-%d", i))
		hasTitle := strings.Contains(fmt.Sprint(conv["title"]), fmt.Sprintf("SYNTH-TITLE-%d", i))
		hasExec := strings.Contains(fmt.Sprint(ev["input"]), fmt.Sprintf("SYNTH-EXEC-%d", i))
		want := map[string][3]bool{config.ModeMetrics: {false, false, false}, config.ModeMessages: {true, true, false}, config.ModeFull: {true, true, true}}[mode]
		if got := [3]bool{hasText, hasTitle, hasExec}; got != want {
			e.t.Fatalf("%s: record %d under %s exposes text/title/exec %v, want %v (message %v, event %v)", when, i, mode, got, want, msg, ev)
		}
	}
	if mode == config.ModeFull {
		if len(e.leaks(db, "SYNTH-EXEC")) == 0 {
			e.t.Fatalf("%s: full mode must deliver the execution content", when)
		}
		raw := false
		for _, l := range e.leaks(db, "SYNTH-MSG") {
			raw = raw || strings.HasPrefix(l, "raw_events_latest")
		}
		if !raw {
			e.t.Fatalf("%s: full mode must deliver raw event payloads", when)
		}
	}
}

// requireCounts verifies the logical results: every record once, token
// totals exact.
func (e *privEnv) requireCounts(db string, want logical, when string) {
	e.t.Helper()
	if got := e.mustRead(db); got != want {
		e.t.Fatalf("%s: logical results %+v, want %+v", when, got, want)
	}
}

// plus is base with n more sensitive conversations (see sensitive), each of
// which appends five source events.
func plus(base logical, n int) logical {
	base.Raw += 5 * n
	base.Turns += n
	base.Input += uint64(1000 * n)
	base.Msgs += n
	base.Events += n
	base.Convs += n
	return base
}

// The original F1 failure: full → metrics while a row of the full mode is
// queued, and that row's retry arrives after the sanitized one.
func TestIntegrationF1StaleRetryAfterSanitizedRecord(t *testing.T) {
	for _, fault := range []string{"fail", "lost"} {
		t.Run(fault, func(t *testing.T) {
			e := newPrivEnv(t, spec(config.ModeFull))
			c := e.collector()
			e.sync(c)
			e.deliverAll(c)
			base := e.mustRead(e.db)

			e.sensitive(1)
			e.sync(c)
			e.queueStale(c, fault)
			if fault == "lost" {
				// The full rows reached ClickHouse; the client never learned it.
				e.requireMode(e.db, config.ModeFull, 1, "before the change")
			}

			e.setPolicy(spec(config.ModeMetrics))
			c = e.collector()
			e.sync(c) // reprocesses every record under the new settings
			e.deliverFresh(c)
			e.requireMode(e.db, config.ModeMetrics, 1, "sanitized rows delivered")
			e.deliverAll(c) // the stale full rows are retried now
			e.requireMode(e.db, config.ModeMetrics, 1, "stale retries delivered after the sanitized rows")

			// The stale rows really arrived after the sanitized ones.
			ins := e.sink.recorded()
			firstSanitized, lastStale := -1, -1
			for i, in := range ins {
				body := gunzip(t, in.body)
				if in.table != model.TableMessages || !strings.Contains(body, `"msg-s1"`) {
					continue
				}
				if strings.Contains(body, "SYNTH-MSG-1") {
					lastStale = i
				} else if firstSanitized < 0 {
					firstSanitized = i
				}
			}
			if firstSanitized < 0 || lastStale < firstSanitized {
				t.Fatalf("stale retry must arrive after the sanitized row: sanitized at %d, stale at %d", firstSanitized, lastStale)
			}
			// Physical retention: the older version is still stored.
			if e.physical(e.db, model.TableMessages, "SYNTH-MSG-1") == 0 {
				t.Fatal("the stale row should be stored (only the latest version is sanitized)")
			}
			msg := e.latest(e.db, model.TableMessages, "message_id", "msg-s1")
			if msg["content_included"].(float64) != 0 || msg["text"] != "" || generationOf(t, msg) != 2 {
				t.Fatalf("latest message: %v", msg)
			}
			e.requireCounts(e.db, plus(base, 1), "after the change")

			// Reconciliations and repeated (forced) backfills change nothing.
			for _, force := range []bool{false, true, true} {
				if _, err := c.Snapshot(ctx, e.src, force, force); err != nil {
					t.Fatal(err)
				}
				e.deliverAll(c)
				e.requireMode(e.db, config.ModeMetrics, 1, fmt.Sprintf("after snapshot force=%v", force))
				e.requireCounts(e.db, plus(base, 1), fmt.Sprintf("after snapshot force=%v", force))
			}
			e.sync(c)
			e.deliverAll(c)
			e.requireMode(e.db, config.ModeMetrics, 1, "after the next sync")
		})
	}
}

// Every transition between the three modes, with a stale row of the earlier
// mode (whose response was lost, so it is retried under a new token) that
// arrives after the reprocessed one.
func TestIntegrationF1ModeTransitions(t *testing.T) {
	modes := []string{config.ModeMetrics, config.ModeMessages, config.ModeFull}
	for _, from := range modes {
		for _, to := range modes {
			if from == to {
				continue
			}
			t.Run(from+"_to_"+to, func(t *testing.T) {
				e := newPrivEnv(t, spec(from))
				c := e.collector()
				e.sync(c)
				e.deliverAll(c)
				base := e.mustRead(e.db)
				e.sensitive(1)
				e.sync(c)
				e.queueStale(c, "lost")
				e.requireMode(e.db, from, 1, "before the change")

				e.setPolicy(spec(to))
				c = e.collector()
				e.sync(c)
				e.deliverFresh(c)
				e.requireMode(e.db, to, 1, "reprocessed rows delivered")
				e.deliverAll(c)
				e.requireMode(e.db, to, 1, "stale retries delivered")
				e.requireCounts(e.db, plus(base, 1), "after the change")

				// D1: the stale entry was retried under a new token, and its rows
				// (sent twice) are counted once.
				tokens := map[string]map[string]bool{} // row line -> tokens it was sent with
				for _, in := range e.sink.recorded() {
					if in.table != model.TableProviderTurns {
						continue
					}
					for _, line := range strings.Split(gunzip(t, in.body), "\n") {
						if strings.Contains(line, `"turn-s1"`) {
							if tokens[line] == nil {
								tokens[line] = map[string]bool{}
							}
							tokens[line][in.token] = true
						}
					}
				}
				renewed := false
				for _, ts := range tokens {
					renewed = renewed || len(ts) > 1
				}
				if !renewed {
					t.Fatalf("the ambiguous provider-turn insert must have been retried under a new token: %v", tokens)
				}
			})
		}
	}
}

// Changes of the redaction settings (custom patterns and the built-in rules)
// in both directions.
func TestIntegrationF1RedactionChanges(t *testing.T) {
	const custom = `SYNTH-CUSTOM-[0-9]+`
	fakeKey := "sk-ant-api03-SYNTHfakeKEYabcdefghijklmnop0123456789"
	cases := []struct {
		name     string
		from, to policySpec
		// stored is what the latest text must and must not contain afterwards.
		has, hasNot string
	}{
		{"add_pattern", spec(config.ModeFull), spec(config.ModeFull, custom), "[REDACTED:custom]", "SYNTH-CUSTOM-4242"},
		{"remove_pattern", spec(config.ModeFull, custom), spec(config.ModeFull), "SYNTH-CUSTOM-4242", "[REDACTED:custom]"},
		{"enable_builtin", policySpec{mode: config.ModeFull}, spec(config.ModeFull), "[REDACTED:anthropic_key]", fakeKey},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newPrivEnv(t, tc.from)
			c := e.collector()
			e.sync(c)
			e.deliverAll(c)
			text := "token SYNTH-CUSTOM-4242 and key " + fakeKey
			e.fix.Message("msg-r", "thread-1", "run-1", "user", text, false)
			e.fix.Item("item-r", "thread-1", "command_execution", "completed", 9, map[string]any{"input": "echo " + text, "output": text})
			e.sync(c)
			e.queueStale(c, "lost")

			e.setPolicy(tc.to)
			c = e.collector()
			e.sync(c)
			e.deliverFresh(c)
			e.deliverAll(c)
			msg := e.latest(e.db, model.TableMessages, "message_id", "msg-r")
			ev := e.latest(e.db, model.TableExecutionEvents, "event_id", "item-r")
			for _, got := range []string{msg["text"].(string), ev["input"].(string), ev["output"].(string)} {
				if !strings.Contains(got, tc.has) || strings.Contains(got, tc.hasNot) {
					t.Fatalf("latest content %q must contain %q and not %q", got, tc.has, tc.hasNot)
				}
			}
			if strings.Contains(tc.hasNot, "SYNTH") || tc.hasNot == fakeKey {
				e.requireNoLeak(e.db, tc.hasNot, "after the change")
			}
		})
	}
}

// Several changes in quick succession, with re-reads interrupted by crashes,
// rows of each settings left queued, and the newest rows delivered first.
func TestIntegrationF1RapidChangesWithInterruptedRereads(t *testing.T) {
	e := newPrivEnv(t, spec(config.ModeFull))
	c := e.collector()
	e.sync(c)
	e.deliverAll(c)
	base := e.mustRead(e.db)
	e.sensitive(1)
	e.sync(c)
	e.queueStale(c, "lost") // generation 1 (full)

	interrupted := func(p policySpec) {
		e.setPolicy(p)
		src := &interruptSource{Source: e.src, after: 3}
		c := e.collectorFor(e.db, e.sink, src)
		if err := c.syncOnce(ctx, src, c.log); !errors.Is(err, errInterrupted) {
			t.Fatalf("expected an interrupted re-read, got %v", err)
		}
	}
	interrupted(spec(config.ModeMetrics)) // generation 2, partly queued
	interrupted(spec(config.ModeFull))    // generation 3, partly queued (full content)
	interrupted(spec(config.ModeMessages))
	c = e.collector()
	e.queueStale(c, "fail") // none of the partial re-reads reached ClickHouse

	e.setPolicy(spec(config.ModeMetrics)) // generation 5, complete
	c = e.collector()
	e.sync(c)
	if gen, _, _ := e.store.Generation(ctx); gen != 5 {
		t.Fatalf("generation %d, want 5", gen)
	}
	e.deliverFresh(c)
	e.requireMode(e.db, config.ModeMetrics, 1, "final settings delivered first")
	e.deliverAll(c)
	e.requireMode(e.db, config.ModeMetrics, 1, "every stale generation delivered afterwards")
	if g := generationOf(t, e.latest(e.db, model.TableMessages, "message_id", "msg-s1")); g != 5 {
		t.Fatalf("latest generation %d, want 5", g)
	}
	e.requireCounts(e.db, plus(base, 1), "after rapid changes")

	// And back to full: the permissive change is applied as completely.
	e.setPolicy(spec(config.ModeFull))
	c = e.collector()
	e.sync(c)
	e.deliverAll(c)
	e.requireMode(e.db, config.ModeFull, 1, "back to full")
	e.requireCounts(e.db, plus(base, 1), "back to full")
}

// The settings change while ClickHouse is unreachable; the collector is
// restarted (local state reopened) during and after the interrupted re-read.
func TestIntegrationF1OfflineChangeAndRestarts(t *testing.T) {
	e := newPrivEnv(t, spec(config.ModeFull))
	c := e.collector()
	e.sync(c)
	e.deliverAll(c)
	base := e.mustRead(e.db)
	e.sensitive(1)
	e.sync(c)
	e.queueStale(c, "lost")

	restart := func() {
		path := e.store.Path()
		if err := e.store.Close(); err != nil {
			t.Fatal(err)
		}
		st, err := spool.Open(path, 64<<20)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { st.Close() })
		e.store = st
	}

	e.sink.set("down")
	e.setPolicy(spec(config.ModeMetrics))
	restart()
	src := &interruptSource{Source: e.src, after: 4}
	c = e.collectorFor(e.db, e.sink, src) // the destination check fails: collection continues offline
	if err := c.syncOnce(ctx, src, c.log); !errors.Is(err, errInterrupted) {
		t.Fatalf("expected an interrupted re-read, got %v", err)
	}
	if _, err := c.deliverRound(ctx, time.Now().Add(48*time.Hour)); err == nil {
		t.Fatal("delivery must fail while the server is down")
	}
	restart()
	c = e.collectorFor(e.db, e.sink, e.src)
	e.sync(c) // completes the re-read offline
	e.sink.set("")

	restart()
	c = e.collector()
	e.deliverFresh(c)
	e.requireMode(e.db, config.ModeMetrics, 1, "after the offline re-read")
	e.deliverAll(c)
	e.requireMode(e.db, config.ModeMetrics, 1, "after the stale retries")
	e.requireCounts(e.db, plus(base, 1), "after the offline change")
	// The database holds only this spool's generations: no needless re-read.
	if gen, _, _ := e.store.Generation(ctx); gen != 2 {
		t.Fatalf("generation %d, want 2", gen)
	}
	if need, _, _ := e.store.NeedsResync(ctx, e.src.ID(), e.pol.Fingerprint()); need {
		t.Fatal("no further re-read needed")
	}
}

// Source revisions while the settings change: the newest revision under the
// newest settings wins, and within one generation newer revisions still win
// over older ones delivered later.
func TestIntegrationF1SourceRevisionsDuringChange(t *testing.T) {
	e := newPrivEnv(t, spec(config.ModeFull))
	c := e.collector()
	e.sensitive(1)
	e.sync(c)
	e.deliverAll(c)

	rev := func(n int) string { return fmt.Sprintf("SYNTH-MSG-1 revision %d %s", n, strings.Repeat("x", n)) }
	e.fix.Message("msg-s1", "thread-s1", "run-s1", "user", rev(1), false)
	e.sync(c)
	e.queueStale(c, "lost") // full revision 1, retried later

	e.setPolicy(spec(config.ModeMetrics))
	c = e.collector()
	e.sync(c) // re-read: revision 1 sanitized
	e.fix.Message("msg-s1", "thread-s1", "run-s1", "user", rev(2), false)
	e.sync(c) // revision 2 sanitized
	e.deliverFresh(c)
	e.deliverAll(c)
	msg := e.latest(e.db, model.TableMessages, "message_id", "msg-s1")
	if msg["text"] != "" || uint64(msg["text_chars"].(float64)) != uint64(len(rev(2))) {
		t.Fatalf("latest must be revision 2, sanitized: %v", msg)
	}
	e.requireMode(e.db, config.ModeMetrics, 1, "after revisions")

	// Within one generation: revision 3 is retried after revision 4 arrived.
	e.fix.Message("msg-s1", "thread-s1", "run-s1", "user", rev(3), false)
	e.sync(c)
	e.queueStale(c, "fail")
	e.fix.Message("msg-s1", "thread-s1", "run-s1", "user", rev(4), false)
	e.sync(c)
	e.deliverFresh(c)
	e.deliverAll(c)
	if msg := e.latest(e.db, model.TableMessages, "message_id", "msg-s1"); uint64(msg["text_chars"].(float64)) != uint64(len(rev(4))) {
		t.Fatalf("latest must be revision 4: %v", msg)
	}

	e.setPolicy(spec(config.ModeFull))
	c = e.collector()
	e.sync(c)
	e.deliverAll(c)
	if msg := e.latest(e.db, model.TableMessages, "message_id", "msg-s1"); msg["text"] != rev(4) {
		t.Fatalf("latest must be revision 4 in full: %v", msg)
	}
}

// Destination isolation: rows of earlier settings queued for one database
// never reach another, rows moved with `destination switch --move-pending`
// cannot override the new database's sanitized rows, and a database switched
// back to is re-read under the current settings.
func TestIntegrationF1DestinationSwitch(t *testing.T) {
	for _, pending := range []string{"move", "keep"} {
		t.Run(pending, func(t *testing.T) {
			e := newPrivEnv(t, spec(config.ModeFull))
			dbA := e.db
			dbB := fmt.Sprintf("vario_f1b_%d", time.Now().UnixNano())
			idB := e.migrate(dbB)
			sinkB := &gateSink{Client: e.clients[dbB]}

			a := e.collector()
			e.sync(a)
			e.deliverAll(a)
			e.sensitive(1)
			e.sync(a)
			e.queueStale(a, "fail") // full rows queued for A, never delivered
			queued := e.stats().Rows

			e.setPolicy(spec(config.ModeMetrics))
			// What `vario destination switch` does.
			res, err := e.store.Resolve(ctx, e.endpoint, dbB, idB)
			if err != nil || res.Previous == nil || res.Previous.Pending != queued {
				t.Fatalf("resolve B: %+v %v", res, err)
			}
			if pending == "move" {
				if n, err := e.store.MovePending(ctx, res.Previous.ID, res.Configured.ID); err != nil || n != queued {
					t.Fatalf("move: %d %v", n, err)
				}
			}
			if err := e.store.Activate(ctx, res.Configured.ID); err != nil {
				t.Fatal(err)
			}
			b := e.collectorFor(dbB, sinkB, e.src)
			e.sync(b)
			e.deliverFresh(b)
			e.deliverAll(b) // with "move", A's stale full rows arrive here last
			e.requireMode(dbB, config.ModeMetrics, 1, "B")
			if n := e.physical(dbB, model.TableMessages, "SYNTH-"); (pending == "move") != (n > 0) {
				t.Fatalf("%s: B stores %d rows with A's queued content", pending, n)
			}
			if n := e.physical(dbA, model.TableMessages, "SYNTH-"); n != 0 {
				t.Fatalf("A must not receive anything while B is active: %d rows", n)
			}

			// Back to A (nothing queued for B): A is re-read under the current settings.
			a = e.collector()
			e.sync(a)
			e.deliverFresh(a)
			e.deliverAll(a) // with "keep", A's own stale full rows arrive here last
			e.requireMode(dbA, config.ModeMetrics, 1, "A after switching back")
			if conv := e.latest(dbA, model.TableConversations, "conversation_id", "thread-1"); conv["title"] != "" {
				t.Fatalf("records delivered to A under full must be sanitized too: %v", conv)
			}
			if got, want := e.mustRead(dbA), e.mustRead(dbB); got != want {
				t.Fatalf("A %+v and B %+v must hold the same logical records", got, want)
			}
		})
	}
}

// The local state is deleted, or restored from an older copy, after the
// database received rows of a later generation under other settings. The
// collector must notice before it delivers and start a generation above
// them; otherwise those rows would outrank the rows of the current settings.
func TestIntegrationF1LocalStateResetOrRestored(t *testing.T) {
	for _, how := range []string{"deleted", "restored"} {
		t.Run(how, func(t *testing.T) {
			e := newPrivEnv(t, spec(config.ModeMetrics))
			e.sensitive(1)
			c := e.collector()
			e.sync(c)
			e.deliverAll(c)
			base := e.mustRead(e.db)
			backup := filepath.Join(t.TempDir(), "backup.db")
			path := e.store.Path()
			if err := e.store.Close(); err != nil {
				t.Fatal(err)
			}
			copyFile(t, path, backup)
			st, err := spool.Open(path, 64<<20)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { st.Close() })
			e.store = st

			// Later generations under other settings reach the database.
			for _, p := range []policySpec{spec(config.ModeFull), spec(config.ModeMessages), spec(config.ModeFull)} {
				e.setPolicy(p)
				c = e.collector()
				e.sync(c)
				e.deliverAll(c)
			}
			e.requireMode(e.db, config.ModeFull, 1, "full delivered")
			if err := e.store.Close(); err != nil {
				t.Fatal(err)
			}

			// The state is replaced, and the collector restarted under metrics.
			if how == "deleted" {
				path = filepath.Join(t.TempDir(), "state.db")
			} else {
				for _, suffix := range []string{"", "-wal", "-shm"} {
					_ = os.Remove(path + suffix)
				}
				copyFile(t, backup, path)
			}
			st, err = spool.Open(path, 64<<20)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { st.Close() })
			e.store = st
			e.setPolicy(spec(config.ModeMetrics))
			c = e.collector()
			gen, _, _ := e.store.Generation(ctx)
			if gen <= 4 {
				t.Fatalf("generation %d must be above the database's 4", gen)
			}
			e.sync(c)
			e.deliverAll(c)
			e.requireMode(e.db, config.ModeMetrics, 1, "after the "+how+" local state")
			e.requireCounts(e.db, base, "after the "+how+" local state")
		})
	}
}

func copyFile(t *testing.T, from, to string) {
	t.Helper()
	b, err := os.ReadFile(from)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(to, b, 0o600); err != nil {
		t.Fatal(err)
	}
}
