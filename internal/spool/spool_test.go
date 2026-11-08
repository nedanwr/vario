package spool

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/nedanwr/vario/internal/model"
)

func open(t *testing.T, maxBytes int64) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.db")
	s, err := Open(path, maxBytes)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, path
}

func msg(id, text string, version uint64) model.Record {
	return model.Record{Table: model.TableMessages, Key: id, Row: &model.MessageRow{
		Scoped:    model.Scoped{Envelope: model.Envelope{SourceID: "src", Version: version}, ConversationID: "c1"},
		MessageID: id, Text: text,
	}}
}

func raw(seq int64) model.Record {
	return model.Record{Table: model.TableRawEvents, Key: "raw", Row: &model.RawEventRow{
		Envelope: model.Envelope{SourceID: "src", Version: 1}, Sequence: seq,
	}}
}

var ctx = context.Background()

func ptr(s string) *string { return &s }

func TestCommitIsAtomicWithCursorAndPersists(t *testing.T) {
	s, path := open(t, 0)
	res, err := s.Commit(ctx, "src", []model.Record{msg("m1", "a", 1), msg("m2", "b", 1), raw(1)}, ptr("c1"), CommitOptions{})
	if err != nil || res.Enqueued != 3 {
		t.Fatalf("commit: %+v %v", res, err)
	}
	s.Close()

	s2, err := Open(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	cur, ok, _ := s2.Cursor(ctx, "src")
	if !ok || cur != "c1" {
		t.Fatalf("cursor after reopen: %q %t", cur, ok)
	}
	st, _ := s2.Stats(ctx)
	if st.Rows != 3 || st.ByTable[model.TableMessages] != 2 || st.ByTable[model.TableRawEvents] != 1 {
		t.Fatalf("stats after reopen: %+v", st)
	}

	// A failing commit (cancelled context) must not advance the cursor.
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := s2.Commit(cctx, "src", []model.Record{msg("m3", "c", 2)}, ptr("c2"), CommitOptions{}); err == nil {
		t.Fatal("expected error from cancelled commit")
	}
	if cur, _, _ := s2.Cursor(ctx, "src"); cur != "c1" {
		t.Fatalf("cursor advanced despite failed commit: %q", cur)
	}
}

func TestFingerprintsSkipUnchanged(t *testing.T) {
	s, _ := open(t, 0)
	opts := CommitOptions{PolicyFingerprint: "p1"}
	if r, _ := s.Commit(ctx, "src", []model.Record{msg("m1", "a", 1), raw(1)}, nil, opts); r.Enqueued != 2 {
		t.Fatalf("first commit: %+v", r)
	}
	// Same content at a newer version is skipped; raw events are never fingerprinted.
	if r, _ := s.Commit(ctx, "src", []model.Record{msg("m1", "a", 2), raw(1)}, nil, opts); r.Enqueued != 1 || r.Skipped != 1 {
		t.Fatalf("unchanged commit: %+v", r)
	}
	if r, _ := s.Commit(ctx, "src", []model.Record{msg("m1", "changed", 3)}, nil, opts); r.Enqueued != 1 {
		t.Fatalf("changed content must enqueue: %+v", r)
	}
	if r, _ := s.Commit(ctx, "src", []model.Record{msg("m1", "changed", 4)}, nil, CommitOptions{PolicyFingerprint: "p2"}); r.Enqueued != 1 {
		t.Fatalf("policy change must enqueue: %+v", r)
	}
	if r, _ := s.Commit(ctx, "src", []model.Record{msg("m1", "changed", 5)}, nil, CommitOptions{PolicyFingerprint: "p2", Force: true}); r.Enqueued != 1 {
		t.Fatalf("force must enqueue: %+v", r)
	}
	if r, _ := s.Commit(ctx, "other", []model.Record{msg("m1", "changed", 5)}, nil, CommitOptions{PolicyFingerprint: "p2"}); r.Enqueued != 1 {
		t.Fatalf("fingerprints are per source: %+v", r)
	}
	if err := s.ResetSource(ctx, "src"); err != nil {
		t.Fatal(err)
	}
	if r, _ := s.Commit(ctx, "src", []model.Record{msg("m1", "changed", 6)}, nil, CommitOptions{PolicyFingerprint: "p2"}); r.Enqueued != 1 {
		t.Fatalf("reset must clear fingerprints: %+v", r)
	}
}

func TestDueAckFail(t *testing.T) {
	s, _ := open(t, 0)
	recs := []model.Record{}
	for i := 0; i < 5; i++ {
		recs = append(recs, msg(string(rune('a'+i)), "x", 1))
	}
	if _, err := s.Commit(ctx, "src", recs, nil, CommitOptions{MaxRowsPerEntry: 2}); err != nil {
		t.Fatal(err)
	}
	all, err := s.Due(ctx, time.Now(), 100)
	if err != nil || len(all) != 3 || all[0].Rows != 2 || all[2].Rows != 1 {
		t.Fatalf("entries: %+v %v", all, err)
	}
	body, err := all[0].Body()
	if err != nil || strings.Count(string(body), "\n") != 2 || !strings.Contains(string(body), `"message_id":"a"`) {
		t.Fatalf("body: %q %v", body, err)
	}
	if limited, _ := s.Due(ctx, time.Now(), 3); len(limited) != 1 {
		t.Fatalf("row limit should cap entries: %d", len(limited))
	}

	if err := s.Fail(ctx, []int64{all[0].ID}, time.Now().Add(time.Hour), "boom"); err != nil {
		t.Fatal(err)
	}
	due, _ := s.Due(ctx, time.Now(), 100)
	if len(due) != 2 || due[0].ID == all[0].ID {
		t.Fatalf("failed entry should wait for its retry time: %+v", due)
	}
	st, _ := s.Stats(ctx)
	if st.FailingEntries != 1 || st.LastError != "boom" {
		t.Fatalf("stats: %+v", st)
	}
	if err := s.Ack(ctx, []int64{due[0].ID, due[1].ID}); err != nil {
		t.Fatal(err)
	}
	next, ok, _ := s.NextDue(ctx)
	if !ok || time.Until(next) < 50*time.Minute {
		t.Fatalf("next due: %v %t", next, ok)
	}
	later, _ := s.Due(ctx, time.Now().Add(2*time.Hour), 100)
	if len(later) != 1 || later[0].Attempts != 1 {
		t.Fatalf("retry entry: %+v", later)
	}
}

func TestBoundedSize(t *testing.T) {
	s, _ := open(t, 1)
	if _, err := s.Commit(ctx, "src", []model.Record{msg("m1", strings.Repeat("x", 100), 1)}, ptr("c1"), CommitOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Commit(ctx, "src", []model.Record{msg("m2", "y", 1)}, ptr("c2"), CommitOptions{}); !errors.Is(err, ErrFull) {
		t.Fatalf("expected ErrFull, got %v", err)
	}
	if cur, _, _ := s.Cursor(ctx, "src"); cur != "c1" {
		t.Fatalf("cursor must not advance when full: %q", cur)
	}
	// A cursor-only commit is still allowed when full.
	if _, err := s.Commit(ctx, "src", nil, ptr("c3"), CommitOptions{}); err != nil {
		t.Fatal(err)
	}
}

func TestLease(t *testing.T) {
	s, _ := open(t, 0)
	if err := s.AcquireLease(ctx, "a", "run", time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := s.AcquireLease(ctx, "a", "run", time.Minute); err != nil {
		t.Fatal("owner should be able to renew")
	}
	if err := s.AcquireLease(ctx, "b", "backfill", time.Minute); !errors.Is(err, ErrLeaseHeld) {
		t.Fatalf("expected ErrLeaseHeld, got %v", err)
	}
	if l, active, _ := s.CurrentLease(ctx); !active || l.Command != "run" {
		t.Fatalf("current lease: %+v %t", l, active)
	}
	if err := s.ReleaseLease(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	if err := s.AcquireLease(ctx, "b", "backfill", time.Millisecond); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	if err := s.AcquireLease(ctx, "c", "run", time.Minute); err != nil {
		t.Fatalf("expired lease should be taken over: %v", err)
	}
}

// A lease left behind by a crashed collector on this host is taken over
// immediately instead of after the TTL.
func TestLeaseOfDeadProcessIsTakenOver(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("process liveness is not probed on Windows")
	}
	s, _ := open(t, 0)
	pid := deadPID(t)
	host, _ := os.Hostname()
	b, _ := json.Marshal(Lease{Owner: "crashed", Host: host, PID: pid, Command: "run", Expires: time.Now().Add(time.Hour)})
	if err := s.SetKV(ctx, leaseKey, string(b)); err != nil {
		t.Fatal(err)
	}
	if _, active, _ := s.CurrentLease(ctx); active {
		t.Error("a dead holder's lease must not be reported as active")
	}
	if err := s.AcquireLease(ctx, "new", "run", time.Minute); err != nil {
		t.Fatalf("lease of dead process should be taken over: %v", err)
	}
	// A live holder on another host is respected until it expires.
	b, _ = json.Marshal(Lease{Owner: "remote", Host: "other-host", PID: pid, Command: "run", Expires: time.Now().Add(time.Hour)})
	_ = s.SetKV(ctx, leaseKey, string(b))
	if err := s.AcquireLease(ctx, "new", "run", time.Minute); !errors.Is(err, ErrLeaseHeld) {
		t.Fatalf("lease from another host must be respected: %v", err)
	}
	// A live local process (this one) is respected.
	b, _ = json.Marshal(Lease{Owner: "live", Host: host, PID: os.Getpid(), Command: "run", Expires: time.Now().Add(time.Hour)})
	_ = s.SetKV(ctx, leaseKey, string(b))
	if err := s.AcquireLease(ctx, "new", "run", time.Minute); !errors.Is(err, ErrLeaseHeld) {
		t.Fatalf("live holder must be respected: %v", err)
	}
}

func deadPID(t *testing.T) int {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^$")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	return cmd.Process.Pid
}
