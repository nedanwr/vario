package cli

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/nedanwr/vario/internal/config"
	"github.com/nedanwr/vario/internal/spool"
)

// keptFixture reproduces the stranded state: rows are queued for a database
// (destination #1, id idA) that is then dropped and recreated (idB), and the
// replacement is accepted with `destination switch --keep-pending`, which
// makes it destination #2. It returns the fake server, both ids and the
// number of kept rows.
func keptFixture(t *testing.T) (ch *fakeCH, idA, idB, queued string) {
	t.Helper()
	fix, ch := replacementFixture(t)
	useFakeService(t, &fakeService{})
	migrateAsAdmin(t)
	idA = ch.destinationID("vario")
	if r := vario(t, "", "backfill", "--source", "t3code"); r.code != 0 || ch.inserted("vario", "messages") != 2 {
		t.Fatalf("initial delivery: %+v", r)
	}
	fix.Message("msg-buffered", "thread-1", "run-1", "assistant", "collected before the replacement", false)
	if r := vario(t, "", "backfill", "--source", "t3code", "--no-deliver"); r.code != 0 {
		t.Fatalf("queue: %+v", r)
	}
	if queued = spooled(t); queued == "0" {
		t.Fatal("expected buffered rows")
	}
	ch.recreate("vario")
	migrateAsAdmin(t)
	idB = ch.destinationID("vario")
	r := vario(t, "", "destination", "switch", "--keep-pending")
	if r.code != 0 {
		t.Fatalf("switch --keep-pending: %+v", r)
	}
	wantAll(t, "switch --keep-pending", r.stdout, queued+" queued rows stay with", "They are not delivered while it is not in use",
		"retry, move or discard them with `vario destination pending 1`")
	if got := kept(t); got != queued || spooled(t) != "0" {
		t.Fatalf("the rows must be kept for the replaced destination: %s kept, %s for the new one", got, spooled(t))
	}
	return ch, idA, idB, queued
}

var keptRe = regexp.MustCompile(`queued\s+(\d+) rows in`)

// kept returns the number of rows kept for destinations that are not in use.
func kept(t *testing.T) string {
	t.Helper()
	r := vario(t, "", "destination", "pending")
	if r.code != 0 {
		t.Fatalf("destination pending: %+v", r)
	}
	if m := keptRe.FindStringSubmatch(r.stdout); m != nil {
		return m[1]
	}
	if !strings.Contains(r.stdout, "No rows are kept for a previous destination.") {
		t.Fatalf("destination pending: %s", r.stdout)
	}
	return "0"
}

// The rows kept by --keep-pending are visible, with their destination's
// identity and counts, everywhere the user looks, and the commands that
// decide about them are named.
func TestKeptRowsAreReported(t *testing.T) {
	_, idA, _, queued := keptFixture(t)
	r := vario(t, "", "destination", "pending")
	wantAll(t, "destination pending", r.stdout, "Rows kept for destinations that are not in use", "#1  ", "database vario (replaced ",
		"destination id  "+idA, "queued          "+queued+" rows in ", "tables          ", "messages=1", "this database was replaced",
		"vario destination pending 1 --retry", "reports destination id "+idA, "vario destination pending 1 --move", "vario destination pending 1 --discard",
		"Nothing is delivered elsewhere or deleted until you decide")
	if strings.Contains(r.stdout, "#2") {
		t.Fatalf("the active destination holds no kept rows: %s", r.stdout)
	}
	one := vario(t, "", "destination", "pending", "1")
	wantAll(t, "destination pending 1", one.stdout, "#1  ", "queued          "+queued+" rows in ")
	if r := vario(t, "", "destination", "pending", "2"); r.code != 0 || !strings.Contains(r.stdout, "queued          nothing") {
		t.Fatalf("destination pending 2: %+v", r)
	}
	wantAll(t, "destination", vario(t, "", "destination").stdout, queued+" rows are queued for", "vario destination pending 1")
	wantAll(t, "status", vario(t, "", "status").stdout, queued+" rows queued for other destinations")
	wantAll(t, "doctor", vario(t, "", "doctor").stdout, queued+" rows are queued for", "vario destination pending")
	wantAll(t, "help", vario(t, "", "destination", "pending", "--help").stdout, "--retry", "--move", "--discard", "--yes")

	// Looking changes nothing, and the collector never touches them.
	if r := vario(t, "", "backfill", "--source", "t3code"); r.code != 0 {
		t.Fatalf("backfill for the new destination: %+v", r)
	}
	if got := kept(t); got != queued {
		t.Fatalf("kept rows after a collector run: %s, had %s", got, queued)
	}

	// Usage errors change nothing either.
	for _, args := range [][]string{
		{"--move"}, {"--yes"}, {"1", "--retry", "--move"}, {"x", "--discard"}, {"9", "--retry"}, {"9"}, {"1", "2"},
	} {
		if r := vario(t, "y\n", append([]string{"destination", "pending"}, args...)...); r.code != ExitUsage {
			t.Fatalf("destination pending %v: %+v", args, r)
		}
	}
	if got := kept(t); got != queued {
		t.Fatalf("usage errors must change nothing: %s", got)
	}
}

// --retry delivers the kept rows only to the database they were queued for:
// not to the one that replaced it, not while it is unreachable. Once it is
// back they arrive there; an interrupted retry is resumed by running it
// again, and a repeated one does nothing.
func TestKeptRowsRetry(t *testing.T) {
	ch, idA, idB, queued := keptFixture(t)
	delivered := func() int {
		n := 0
		for _, table := range []string{"messages", "raw_events", "conversations", "runs", "projects", "sources", "provider_turns"} {
			n += ch.inserted("vario", table)
		}
		return n
	}
	before := delivered()

	// The configured database is the replacement.
	r := vario(t, "", "destination", "pending", "1", "--retry")
	if r.code != ExitFailure || delivered() != before || kept(t) != queued {
		t.Fatalf("retry at the replacing database: %+v", r)
	}
	wantAll(t, "retry at the replacing database", r.stderr, "recorded destination id "+idA, "found "+idB, "nothing was sent and nothing was changed",
		"vario destination pending 1 --move", "--discard")

	// Offline.
	ch.setDown(true)
	r = vario(t, "", "destination", "pending", "1", "--retry")
	if r.code != ExitIncomplete || kept(t) != queued {
		t.Fatalf("retry while offline: %+v", r)
	}
	wantAll(t, "retry while offline", r.stderr, "retry incomplete, "+queued+" rows remain queued", "run this command again")
	ch.setDown(false)

	// The original database is restored, but delivery breaks off.
	ch.setDestinationID("vario", idA)
	ch.failInserts("raw_events", 1)
	r = vario(t, "", "destination", "pending", "1", "--retry")
	left := kept(t)
	if r.code != ExitIncomplete || left == "0" || left == queued || ch.inserted("vario", "messages") != 1 {
		t.Fatalf("interrupted retry: %+v (%s of %s rows left)", r, left, queued)
	}
	wantAll(t, "interrupted retry", r.stdout, "✓ delivered ")
	wantAll(t, "interrupted retry", r.stderr, "retry incomplete, "+left+" rows remain queued", "simulated delivery failure")
	wantAll(t, "pending after a failure", vario(t, "", "destination", "pending").stdout, "last failure    ", "simulated delivery failure")

	// Run again (a new process): the rest is delivered.
	r = vario(t, "", "destination", "pending", "1", "--retry")
	if r.code != 0 || kept(t) != "0" {
		t.Fatalf("resumed retry: %+v", r)
	}
	wantAll(t, "resumed retry", r.stdout, "✓ delivered "+left+" rows", "Nothing is left queued for it", "active destination and all cursors are unchanged")
	if n := delivered() - before; fmt.Sprint(n) != queued || ch.inserted("vario", "messages") != 1 {
		t.Fatalf("every kept row must arrive exactly once: %d of %s rows, %d messages", n, queued, ch.inserted("vario", "messages"))
	}

	// Repeated: nothing to do, nothing sent.
	r = vario(t, "", "destination", "pending", "1", "--retry")
	if r.code != 0 || !strings.Contains(r.stdout, "nothing to do") || fmt.Sprint(delivered()-before) != queued {
		t.Fatalf("repeated retry: %+v", r)
	}
	// The destinations are as they were: #2 active, #1 with its cursor.
	out := vario(t, "", "destination").stdout
	wantAll(t, "destination after the retry", out, "[active, configured]", "queued rows 0 · sources with a cursor 1 · destination id "+idA)
}

// --move re-binds the kept rows to the active destination after an explicit
// confirmation that names the consequences. It needs no ClickHouse, is
// refused while a collector runs, and the new database ends up complete.
func TestKeptRowsMove(t *testing.T) {
	ch, _, _, queued := keptFixture(t)
	unchanged := func(what string, r result) {
		t.Helper()
		if kept(t) != queued || spooled(t) != "0" {
			t.Fatalf("%s must change nothing: %+v", what, r)
		}
	}

	// Not under a running collector.
	host, _ := os.Hostname()
	cfgPath := os.Getenv(config.EnvConfig)
	holdLease(t, cfgPath, spool.Lease{Owner: "fg", Host: host, PID: os.Getpid(), Command: "run", Expires: time.Now().Add(time.Hour)})
	r := vario(t, "", "destination", "pending", "1", "--move", "--yes")
	if r.code != ExitFailure {
		t.Fatalf("move under a collector: %+v", r)
	}
	wantAll(t, "move under a collector", r.stderr, "a collector is running on this computer", "Nothing was changed", "vario destination pending 1 --move")
	unchanged("a refused move", r)
	holdLease(t, cfgPath, spool.Lease{Owner: "old", Host: "other-host", PID: 42, Command: "run", Expires: time.Now().Add(-time.Minute)})

	// The consequences are stated; without an answer nothing happens.
	r = vario(t, "", "destination", "pending", "1", "--move")
	if r.code == 0 {
		t.Fatalf("move without confirmation: %+v", r)
	}
	wantAll(t, "move consequences", r.stdout, "Move "+queued+" queued rows from ", "to the active destination", "not to the database they were collected for",
		"keep their keys and versions", "The replaced database never receives them", "cursors and fingerprints are deleted", "This cannot be undone")
	unchanged("an unanswered move", r)
	r = vario(t, "n\n", "destination", "pending", "1", "--move")
	if r.code != 0 || !strings.Contains(r.stdout, "Nothing was changed.") {
		t.Fatalf("declined move: %+v", r)
	}
	unchanged("a declined move", r)

	// Confirmed, with ClickHouse unreachable: a local change only.
	ch.setDown(true)
	r = vario(t, "y\n", "destination", "pending", "1", "--move")
	if r.code != 0 {
		t.Fatalf("move: %+v", r)
	}
	wantAll(t, "move", r.stdout, "✓ "+queued+" queued rows will be delivered to ", "Start the collector to deliver them")
	if kept(t) != "0" || spooled(t) != queued {
		t.Fatalf("moved rows must be queued for the active destination: %s kept, %s spooled", kept(t), spooled(t))
	}
	wantAll(t, "destination after the move", vario(t, "", "destination").stdout, "queued rows 0 · sources with a cursor 0")

	// Repeated: nothing is moved or duplicated.
	r = vario(t, "", "destination", "pending", "1", "--move", "--yes")
	if r.code != 0 || !strings.Contains(r.stdout, "nothing to do") || spooled(t) != queued {
		t.Fatalf("repeated move: %+v", r)
	}

	// The collector delivers them with the full history of the new database.
	ch.setDown(false)
	if r := vario(t, "", "backfill", "--source", "t3code"); r.code != 0 || spooled(t) != "0" {
		t.Fatalf("delivery after the move: %+v", r)
	}
	if n := ch.inserted("vario", "messages"); n < 3 {
		t.Fatalf("the new database is incomplete: %d message rows", n)
	}
}

// --discard deletes the kept rows after an explicit confirmation and nothing
// else; the active destination is still synchronized in full.
func TestKeptRowsDiscard(t *testing.T) {
	ch, _, _, queued := keptFixture(t)
	r := vario(t, "", "destination", "pending", "1", "--discard")
	if r.code == 0 || kept(t) != queued {
		t.Fatalf("discard without confirmation: %+v", r)
	}
	wantAll(t, "discard consequences", r.stdout, "Delete "+queued+" queued rows for ", "never delivered anywhere", "is lost",
		"The replaced database never receives them", "This cannot be undone")
	if r = vario(t, "\n", "destination", "pending", "1", "--discard"); r.code != 0 || !strings.Contains(r.stdout, "Nothing was changed.") || kept(t) != queued {
		t.Fatalf("declined discard: %+v", r)
	}

	// Rows queued for the active destination are not affected, and cannot be
	// discarded this way.
	if r := vario(t, "", "backfill", "--source", "t3code", "--no-deliver"); r.code != 0 {
		t.Fatalf("queue for the new destination: %+v", r)
	}
	own := spooled(t)
	if r = vario(t, "", "destination", "pending", "2", "--discard", "--yes"); r.code != ExitUsage || !strings.Contains(r.stderr, "is the active destination") || spooled(t) != own {
		t.Fatalf("discard of the active destination's rows: %+v", r)
	}

	ch.setDown(true) // no ClickHouse needed
	r = vario(t, "", "destination", "pending", "1", "--discard", "--yes")
	if r.code != 0 || !strings.Contains(r.stdout, "✓ deleted "+queued+" queued rows") || kept(t) != "0" || spooled(t) != own {
		t.Fatalf("discard: %+v", r)
	}
	if r = vario(t, "", "destination", "pending", "1", "--discard", "--yes"); r.code != 0 || !strings.Contains(r.stdout, "nothing to do") || spooled(t) != own {
		t.Fatalf("repeated discard: %+v", r)
	}
	ch.setDown(false)
	if r := vario(t, "", "backfill", "--source", "t3code"); r.code != 0 || spooled(t) != "0" || ch.inserted("vario", "messages") < 3 {
		t.Fatalf("the new database must still receive the complete history: %+v (%d messages)", r, ch.inserted("vario", "messages"))
	}
}

// Rows kept for a destination that was not replaced but is no longer
// configured: a retry never uses the configured database for them, a move
// needs the configured destination to be the active one, and both work
// without that destination being reachable.
func TestKeptRowsOfAnotherDestination(t *testing.T) {
	switchFixture(t)
	useFakeService(t, &fakeService{})
	if r := vario(t, "", "destination", "switch", "--keep-pending"); r.code != 0 {
		t.Fatalf("switch: %+v", r)
	}
	queued := kept(t)
	if queued == "0" {
		t.Fatal("expected kept rows")
	}
	wantAll(t, "destination pending", vario(t, "", "destination", "pending").stdout, "this destination is not the configured one",
		"configure it again first (clickhouse.url, clickhouse.database)")

	r := vario(t, "", "destination", "pending", "1", "--retry")
	if r.code != ExitUsage || kept(t) != queued {
		t.Fatalf("retry at another database: %+v", r)
	}
	wantAll(t, "retry at another database", r.stderr, "the configuration points to", "database other", "Nothing was changed", "configure that database again")

	// Configured again (ClickHouse is unreachable in this fixture): the retry
	// is attempted there and keeps everything; a move has no unambiguous target.
	if r := vario(t, "", "config", "set", "clickhouse.database", "vario"); r.code != 0 {
		t.Fatal(r)
	}
	if r = vario(t, "", "destination", "pending", "1", "--retry"); r.code != ExitIncomplete || kept(t) != queued {
		t.Fatalf("retry while unreachable: %+v", r)
	}
	wantAll(t, "retry while unreachable", r.stderr, "retry incomplete, "+queued+" rows remain queued")
	if r = vario(t, "", "destination", "pending", "1", "--move", "--yes"); r.code != ExitFailure || !strings.Contains(r.stderr, "is not the active one") || kept(t) != queued {
		t.Fatalf("move while another destination is configured: %+v", r)
	}

	if r := vario(t, "", "config", "set", "clickhouse.database", "other"); r.code != 0 {
		t.Fatal(r)
	}
	r = vario(t, "", "destination", "pending", "1", "--move", "--yes")
	if r.code != 0 || kept(t) != "0" || spooled(t) != queued {
		t.Fatalf("move: %+v", r)
	}
	wantAll(t, "move", r.stdout, "its cursors and fingerprints are reset", "every source is read again in full")
}
