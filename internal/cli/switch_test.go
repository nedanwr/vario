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
