package clickhouse

import (
	"errors"
	"strings"
	"testing"
)

// The checkpoint is read and written with the operational client only: one
// SELECT and one single-row insert with the settings of a delivery.
func TestCheckpointReadAndWrite(t *testing.T) {
	f, srv := newFake(t)
	f.tables, f.columns, f.meta = completeSchema("vario")
	c := testClient(t, srv.URL)

	seq, found, err := c.ReadCheckpoint(bg, "w1")
	if err != nil || found || seq != 0 {
		t.Fatalf("no checkpoint yet: %d %v %v", seq, found, err)
	}
	if err := c.WriteCheckpoint(bg, "w1", 7); err != nil {
		t.Fatal(err)
	}
	if err := c.WriteCheckpoint(bg, "w2", 3); err != nil {
		t.Fatal(err)
	}
	if seq, found, err = c.ReadCheckpoint(bg, "w1"); err != nil || !found || seq != 7 {
		t.Fatalf("checkpoint of w1: %d %v %v", seq, found, err)
	}
	// Writers are independent: several collectors share the table.
	if seq, _, _ = c.ReadCheckpoint(bg, "w2"); seq != 3 {
		t.Fatalf("checkpoint of w2: %d", seq)
	}
	// The highest number counts, in whatever order rows arrive.
	if err := c.WriteCheckpoint(bg, "w1", 5); err != nil {
		t.Fatal(err)
	}
	if seq, _, _ = c.ReadCheckpoint(bg, "w1"); seq != 7 {
		t.Fatalf("a lower number must not lower the checkpoint: %d", seq)
	}
	if f.cpBodies[0] != "{\"writer_id\":\"w1\",\"seq\":7}\n" {
		t.Fatalf("checkpoint row: %q", f.cpBodies[0])
	}
	// The write is a synchronous insert without a deduplication token, so its
	// acknowledgement means the row was written and no write is dropped.
	var write, read bool
	for _, p := range f.cpParams {
		if strings.HasPrefix(p.Get("query"), "INSERT INTO `vario`.`vario_checkpoints`") {
			write = true
			if p.Get("async_insert") != "0" || p.Get("insert_deduplication_token") != "" {
				t.Fatalf("checkpoint insert settings: %v", p)
			}
		} else {
			read = true
			if p.Get("select_sequential_consistency") != "1" {
				t.Fatalf("checkpoint read settings: %v", p)
			}
		}
	}
	if !read || !write {
		t.Fatal("expected a checkpoint read and write")
	}
	// Nothing else was changed, and the writer id cannot break out of the query.
	if m := f.mutations(); len(m) != 0 {
		t.Fatalf("unexpected statements: %v", m)
	}
	if _, _, err := c.ReadCheckpoint(bg, "w' OR 1 = 1 --"); err != nil {
		t.Fatalf("a quoted writer id must stay one literal: %v", err)
	}
}

// A session that may not change settings reads the checkpoint without the
// consistency setting; other failures are returned, never read as "no
// checkpoint", and a failed write is an error.
func TestCheckpointFailuresAreReported(t *testing.T) {
	f, srv := newFake(t)
	f.tables, f.columns, f.meta = completeSchema("vario")
	f.checkpoints["w1"] = 4
	c := testClient(t, srv.URL)

	for _, code := range []int{codeSettingLimit, codeReadonly, codeUnknownSetting} {
		f.cpSettingErr = code
		if seq, found, err := c.ReadCheckpoint(bg, "w1"); err != nil || !found || seq != 4 {
			t.Fatalf("code %d: the read must fall back to the default settings: %d %v %v", code, seq, found, err)
		}
	}
	f.cpSettingErr = 0
	f.cpReadErr = 60
	var e *Error
	if _, found, err := c.ReadCheckpoint(bg, "w1"); !errors.As(err, &e) || e.Code != 60 || found {
		t.Fatalf("a failed read must be an error: %v", err)
	}
	f.cpReadErr = 0
	f.cpWriteErr = 497
	if err := c.WriteCheckpoint(bg, "w1", 5); !errors.As(err, &e) || Classify(err) != KindPermission {
		t.Fatalf("a failed write must be an error: %v", err)
	}
	if seq, _, _ := c.ReadCheckpoint(bg, "w1"); seq != 4 {
		t.Fatalf("a failed write must not change the checkpoint: %d", seq)
	}
}
