package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/nedanwr/vario/internal/config"
	"github.com/nedanwr/vario/internal/privacy"
	"github.com/nedanwr/vario/internal/spool"
)

// holdLease records a collector lease in the state of the configuration at cfgPath.
func holdLease(t *testing.T, cfgPath string, l spool.Lease) {
	t.Helper()
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	store, err := openStore(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	b, _ := json.Marshal(l)
	if err := store.SetKV(context.Background(), "collector.lease", string(b)); err != nil {
		t.Fatal(err)
	}
}

// switchFixture queues rows for the configured destination and then changes
// the database, so that `destination switch` has work to do.
func switchFixture(t *testing.T) string {
	t.Helper()
	_, cfgPath := initFixture(t)
	if r := vario(t, "", "backfill", "--source", "t3code", "--no-deliver"); r.code != 0 {
		t.Fatalf("backfill: %+v", r)
	}
	if r := vario(t, "", "config", "set", "clickhouse.database", "other"); r.code != 0 {
		t.Fatalf("config set: %+v", r)
	}
	return cfgPath
}

func TestDestinationSwitchFailsFastWhileCollectorRuns(t *testing.T) {
	cfgPath := switchFixture(t)
	host, _ := os.Hostname()
	live := spool.Lease{Owner: "fg", Host: host, PID: os.Getpid(), Command: "run", Expires: time.Now().Add(time.Hour)}
	before := vario(t, "", "destination").stdout

	// A foreground collector on this computer.
	holdLease(t, cfgPath, live)
	start := time.Now()
	r := vario(t, "", "destination", "switch", "--move-pending")
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("switch must fail promptly, took %v", took)
	}
	want := []string{"a collector is running on this computer (`vario run`, pid " + fmt.Sprint(os.Getpid()) + ")", "Nothing was changed",
		"Ctrl-C in its terminal", "vario destination switch --move-pending"}
	for _, w := range want {
		if r.code != ExitFailure || !strings.Contains(r.stderr, w) {
			t.Fatalf("missing %q: %+v", w, r)
		}
	}
	if after := vario(t, "", "destination").stdout; after != before {
		t.Fatalf("a refused switch must not change any state:\n%s\n---\n%s", before, after)
	}

	// The background service: the stop/switch/start sequence is given.
	useFakeService(t, &fakeService{installed: true, running: true, pid: os.Getpid(), args: []string{"run", "--config", cfgPath}})
	r = vario(t, "", "destination", "switch")
	for _, w := range []string{"the background service is running the collector", "vario service uninstall", "vario destination switch [--keep-pending | --move-pending | --discard-pending]", "vario service install"} {
		if r.code != ExitFailure || !strings.Contains(r.stderr, w) {
			t.Fatalf("missing %q: %+v", w, r)
		}
	}

	// A collector on another host sharing the state directory.
	holdLease(t, cfgPath, spool.Lease{Owner: "remote", Host: "other-host", PID: 42, Command: "run", Expires: time.Now().Add(time.Hour)})
	if r = vario(t, "", "destination", "switch", "--keep-pending"); r.code != ExitFailure || !strings.Contains(r.stderr, "on host other-host") {
		t.Fatalf("remote holder: %+v", r)
	}

	// An expired lease is stale and does not block.
	holdLease(t, cfgPath, spool.Lease{Owner: "old", Host: "other-host", PID: 42, Command: "run", Expires: time.Now().Add(-time.Minute)})
	useFakeService(t, &fakeService{})
	if r = vario(t, "", "destination", "switch", "--keep-pending"); r.code != 0 || !strings.Contains(r.stdout, "active destination") {
		t.Fatalf("a stale lease must not block: %+v", r)
	}
}

// C-1: moving or deleting queued rows resets the previous destination's
// cursors, and the command says so; keeping them keeps its state.
func TestDestinationSwitchResetsPreviousStateWhenRowsLeaveIt(t *testing.T) {
	for flag, reset := range map[string]bool{"--move-pending": true, "--discard-pending": true, "--keep-pending": false} {
		t.Run(flag, func(t *testing.T) {
			switchFixture(t)
			useFakeService(t, &fakeService{})
			if out := vario(t, "", "destination").stdout; !strings.Contains(out, "sources with a cursor 1") {
				t.Fatalf("the previous destination should have a cursor: %s", out)
			}
			r := vario(t, "", "destination", "switch", flag, "--yes")
			if r.code != 0 || strings.Contains(r.stdout, "its cursors were reset") != reset {
				t.Fatalf("switch %s: %+v", flag, r)
			}
			if out := vario(t, "", "destination").stdout; strings.Contains(out, "sources with a cursor 1") == reset {
				t.Fatalf("after %s: %s", flag, out)
			}
		})
	}
}

// Rows moved into a destination that is already synchronized (by `destination
// switch --move-pending` or `destination pending --move`) reset it: its next
// sync re-reads every source in full, forced, so that the moved rows, which
// may be of earlier privacy settings, cannot stay the latest version of
// records its fingerprints would skip (F1).
func TestMoveIntoSynchronizedDestinationResetsIt(t *testing.T) {
	for _, how := range []string{"switch", "pending"} {
		t.Run(how, func(t *testing.T) {
			_, ch := replacementFixture(t)
			cfgPath := os.Getenv(config.EnvConfig)
			useFakeService(t, &fakeService{})
			migrateAsAdmin(t)
			// A complete re-read (with the event history) synchronizes the destination.
			if r := vario(t, "", "backfill", "--source", "t3code", "--force"); r.code != 0 || ch.inserted("vario", "messages") == 0 {
				t.Fatalf("backfill: %+v", r)
			}
			if r := vario(t, "", "config", "set", "clickhouse.database", "other"); r.code != 0 {
				t.Fatalf("config set: %+v", r)
			}
			if r := vario(t, "", "destination", "switch", "--keep-pending"); r.code != 0 {
				t.Fatalf("switch to other: %+v", r)
			}
			if r := vario(t, "", "backfill", "--source", "t3code", "--no-deliver"); r.code != 0 || spooled(t) == "0" {
				t.Fatalf("queue for other: %+v", r)
			}
			if r := vario(t, "", "config", "set", "clickhouse.database", "vario"); r.code != 0 {
				t.Fatalf("config set: %+v", r)
			}
			needs := func() (need, force bool) {
				t.Helper()
				cfg, err := config.Load(cfgPath)
				if err != nil {
					t.Fatal(err)
				}
				store, err := openStore(cfg)
				if err != nil {
					t.Fatal(err)
				}
				defer store.Close()
				d, ok, err := store.Lookup(context.Background(), cfg.ClickHouse.Endpoint(), "vario")
				if err != nil || !ok {
					t.Fatalf("lookup: %v %v", ok, err)
				}
				store.SelectDestination(d.ID)
				policy, err := privacy.New(cfg.Privacy)
				if err != nil {
					t.Fatal(err)
				}
				srcs, err := buildSources(cfg, policy, "t3code")
				if err != nil || len(srcs) != 1 {
					t.Fatalf("sources: %v %v", srcs, err)
				}
				defer srcs[0].Close()
				need, force, err = store.NeedsResync(context.Background(), srcs[0].ID(), policy.Fingerprint())
				if err != nil {
					t.Fatal(err)
				}
				return need, force
			}
			if how == "switch" {
				if r := vario(t, "", "destination", "switch", "--move-pending", "--yes"); r.code != 0 {
					t.Fatalf("switch --move-pending: %+v", r)
				}
			} else {
				if r := vario(t, "", "destination", "switch", "--keep-pending"); r.code != 0 {
					t.Fatalf("switch --keep-pending: %+v", r)
				}
				if _, force := needs(); force {
					t.Fatal("keeping the rows must not reset the destination")
				}
				if r := vario(t, "", "destination", "pending", "2", "--move", "--yes"); r.code != 0 {
					t.Fatalf("pending --move: %+v", r)
				}
			}
			if need, force := needs(); !need || !force {
				t.Fatalf("the destination the rows were moved to needs a forced re-read: need %v force %v", need, force)
			}
			if r := vario(t, "", "backfill", "--source", "t3code", "--force"); r.code != 0 || spooled(t) != "0" {
				t.Fatalf("delivery after the move: %+v", r)
			}
			if need, _ := needs(); need {
				t.Fatal("the backfill completes the re-read")
			}
		})
	}
}
