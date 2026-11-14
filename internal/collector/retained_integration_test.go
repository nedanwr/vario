package collector

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/nedanwr/vario/internal/spool"
)

// Rows kept with `vario destination switch --keep-pending` after a database
// replacement are not stranded: against real ClickHouse they can be retried
// at the original database once it is back (and only there), moved to the
// database that replaced it, or discarded. In every case each logical record
// is counted once and the active database stays complete.
func TestIntegrationKeptRowsAfterReplacement(t *testing.T) {
	for _, mode := range []string{"retry", "move", "discard"} {
		t.Run(mode, func(t *testing.T) {
			e := newRecoveryEnv(t)
			db := fmt.Sprintf("vario_kept_%s_%d", mode, time.Now().UnixNano())
			saved := db + "_orig"
			idA := e.migrate(db)
			admin := e.clients[db]
			t.Cleanup(func() { _ = admin.Exec(context.Background(), "DROP DATABASE IF EXISTS `"+saved+"`") })

			// The original database holds the history so far; newer records
			// are queued for it but not delivered.
			a := e.prepared(db)
			e.syncAndDrain(a)
			delivered := e.mustRead(db)
			e.moreHistory(1)
			if err := a.syncOnce(ctx, e.src, a.log); err != nil {
				t.Fatal(err)
			}
			st, err := e.store.Stats(ctx)
			if err != nil || st.Rows == 0 {
				t.Fatalf("expected rows queued for the original database: %+v %v", st, err)
			}
			queued := st.Rows

			// It is replaced by an empty one (the original is set aside), and
			// the replacement is accepted with --keep-pending.
			if err := admin.Exec(ctx, "RENAME DATABASE `"+db+"` TO `"+saved+"`"); err != nil {
				t.Fatal(err)
			}
			idB := e.migrate(db)
			if idB == idA {
				t.Fatal("the replacing database must have its own destination id")
			}
			res, err := e.store.Resolve(ctx, e.endpoint, db, idB)
			if err != nil || !res.Replaced {
				t.Fatalf("resolve: %+v %v", res, err)
			}
			oldID := res.Configured.ID
			repl, err := e.store.Replace(ctx, oldID, idB)
			if err != nil {
				t.Fatal(err)
			}
			if err := e.store.Activate(ctx, repl.ID); err != nil {
				t.Fatal(err)
			}
			kept := func() spool.Destination {
				t.Helper()
				d, ok, err := e.store.Destination(ctx, oldID)
				if err != nil || !ok {
					t.Fatalf("destination: %v %v", ok, err)
				}
				return d
			}
			e.syncAndDrain(e.prepared(db))
			complete := e.mustRead(db)
			if complete.Turns != delivered.Turns+1 || complete.Msgs != delivered.Msgs+1 || complete.Convs != delivered.Convs+1 {
				t.Fatalf("the replacing database must receive the full history: %+v (the original had %+v)", complete, delivered)
			}
			old := kept()
			if old.Pending != queued || old.Cursors == 0 || old.Retired.IsZero() {
				t.Fatalf("the rows must be kept for the replaced database: %+v", old)
			}

			// They are never retried at the database that replaced theirs.
			dr, err := e.collector(db).DeliverRetained(ctx, old)
			if !errors.Is(err, spool.ErrIdentityMismatch) || dr.Delivered != 0 || kept().Pending != queued {
				t.Fatalf("retry at the replacing database: %+v %v", dr, err)
			}
			if got := e.mustRead(db); got != complete {
				t.Fatalf("a refused retry must not deliver anything: %+v", got)
			}

			switch mode {
			case "retry":
				// The original database is restored in its place.
				if err := admin.Exec(ctx, "DROP DATABASE `"+db+"`"); err != nil {
					t.Fatal(err)
				}
				if err := admin.Exec(ctx, "RENAME DATABASE `"+saved+"` TO `"+db+"`"); err != nil {
					t.Fatal(err)
				}
				if got := e.mustRead(db); got != delivered {
					t.Fatalf("the restored database: %+v, want %+v", got, delivered)
				}
				dr, err := e.collector(db).DeliverRetained(ctx, old)
				if err != nil || int64(dr.Delivered) != queued || dr.Remaining != 0 {
					t.Fatalf("retry at the original database: %+v %v", dr, err)
				}
				got := e.mustRead(db)
				if got.Turns != complete.Turns || got.Msgs != complete.Msgs || got.Convs != complete.Convs || got.Events != complete.Events || got.Input != complete.Input {
					t.Fatalf("the original database must be complete, each record once: %+v, want %+v", got, complete)
				}
				// Repeated: nothing is sent again.
				if dr, err = e.collector(db).DeliverRetained(ctx, kept()); err != nil || dr.Delivered != 0 {
					t.Fatalf("repeated retry: %+v %v", dr, err)
				}
				if again := e.mustRead(db); again != got {
					t.Fatalf("a repeated retry changed the archive: %+v, had %+v", again, got)
				}
				if d := kept(); d.Pending != 0 || d.Cursors != old.Cursors {
					t.Fatalf("the retried destination keeps its state: %+v", d)
				}
			case "move", "discard":
				var n int64
				if mode == "move" {
					n, err = e.store.MovePending(ctx, oldID, repl.ID)
				} else {
					n, err = e.store.DiscardPending(ctx, oldID)
				}
				if err != nil || n != queued {
					t.Fatalf("%s: %d of %d rows, %v", mode, n, queued, err)
				}
				if d := kept(); d.Pending != 0 || d.Cursors != 0 {
					t.Fatalf("after %s: %+v", mode, d)
				}
				c := e.prepared(db)
				if st, _ := e.store.Stats(ctx); (mode == "move") != (st.Rows == queued) {
					t.Fatalf("%s: %d rows queued for the active destination, %d were kept", mode, st.Rows, queued)
				}
				e.syncAndDrain(c)
				if got := e.mustRead(db); got != complete {
					t.Fatalf("after %s the archive must be complete with each record once: %+v, want %+v", mode, got, complete)
				}
				// Repeated: nothing is left to move or delete.
				if n, err := e.store.MovePending(ctx, oldID, repl.ID); err != nil || n != 0 {
					t.Fatalf("repeated: %d %v", n, err)
				}
			}
			if a, ok, _ := e.store.Active(ctx); !ok || a.ID != repl.ID {
				t.Fatalf("the active destination changed: %+v", a)
			}
		})
	}
}
