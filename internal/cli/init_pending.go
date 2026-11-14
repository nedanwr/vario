package cli

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/nedanwr/vario/internal/config"
	"github.com/nedanwr/vario/internal/storage/clickhouse"
)

// ---- passwords of users created by guided setup ---------------------------------
//
// A user created by guided setup must never end up with a password nobody
// has. The password is written to the pending credentials file before CREATE
// USER is sent and removed from it only once a saved configuration uses it.
// See pendingCredentials.

// reviewPending loads the pending credentials and, interactively, offers to
// finish the setup of a user an earlier run created.
func (w *wizard) reviewPending() error {
	w.pend, w.pendErr = loadPendingStore(w.pendPath)
	if w.pendErr != nil {
		w.printf("! %v\n\n", w.pendErr)
		return nil
	}
	for _, e := range append([]pendingCredentials(nil), w.pend.Users...) {
		// A configuration already uses it (setup stopped right after saving).
		if w.prev != nil && e.is(w.prev.ClickHouse.URL, w.prev.ClickHouse.Username) && storedPassword(w.prev) == e.Password {
			if err := w.pend.drop(e.Endpoint, e.User); err == nil {
				w.printf("✓ The configuration already uses the password of %s; its setup copy was removed.\n\n", e.User)
			}
			continue
		}
		w.pendFound[pendKey(e.Endpoint, e.User)] = true
		if !w.p.interactive || w.resumed != nil {
			fmt.Fprintf(w.a.stderr, "! An earlier `vario init` created ClickHouse %s but did not finish.\n"+
				"  Its password is kept in %s; run `vario init` interactively to finish the setup with it.\n", e, config.DisplayPath(w.pendPath))
			continue
		}
		w.printf("An earlier `vario init` created ClickHouse %s,\nbut no configuration using it was saved. Its password is kept in %s (readable only by you).\n",
			e, config.DisplayPath(w.pendPath))
		c, err := w.p.choose("", []string{
			"Finish the setup with this user (recommended)",
			"Continue without it (the password stays in the file for later)",
			"Delete the saved password",
		}, 0)
		if err != nil {
			return err
		}
		w.printf("\n")
		switch c {
		case 0:
			e := e
			w.resumed = &e
			w.cfg.ClickHouse.URL, w.cfg.ClickHouse.Database, w.cfg.ClickHouse.Username = e.Endpoint, e.Database, e.User
			if config.InsecureRemote(e.Endpoint) {
				w.cfg.ClickHouse.AllowInsecureHTTP = true
			}
			w.pwAction, w.pwValue, w.pwGenerated = pwSet, e.Password, e.Generated
		case 2:
			w.printf("User %s stays in ClickHouse. Without this password it cannot be used until an administrator\n"+
				"sets a new one (ALTER USER %s IDENTIFIED BY '...') or removes it (DROP USER %s).\n", e.User, e.User, e.User)
			ok, err := w.p.confirm("Delete the saved password anyway?", false)
			if err != nil {
				return err
			}
			w.printf("\n")
			if ok {
				if err := w.pend.drop(e.Endpoint, e.User); err != nil {
					return err
				}
				delete(w.pendFound, pendKey(e.Endpoint, e.User))
			}
		}
	}
	return nil
}

// storedPassword returns the password in cfg's password file ("" if none).
func storedPassword(cfg *config.Config) string {
	b, err := os.ReadFile(cfg.PasswordPath())
	if err != nil {
		return ""
	}
	return strings.TrimRight(string(b), "\r\n")
}

// resumeOrConnect runs the connection steps, or, when finishing the setup of
// a user created earlier, verifies that user first.
func (w *wizard) resumeOrConnect() error {
	if w.resumed == nil {
		return w.connectionSteps()
	}
	w.setup = "resumed"
	w.dbState, w.userState, w.privState, w.schemaState = "", "dedicated user created by an earlier setup", "", ""
	w.printf("Verifying the login as %s at %s:\n", w.resumed.User, w.resumed.Endpoint)
	rep, err := w.runCheck()
	if err == nil && rep.Ready() {
		w.printf("\n")
		return nil
	}
	if clickhouse.Classify(err) == clickhouse.KindAuth {
		w.printf("\n  The login failed. If setup was interrupted before ClickHouse created the user, guided setup can\n" +
			"  create it now with the saved password (choose \"Create a dedicated Vario user\" with the same name).\n")
	}
	c, cerr := w.p.choose("\nThe login is not ready yet. What would you like to do?", []string{
		"Continue with the connection setup (guided setup can create the schema or grant missing privileges; the saved password is offered for this user)",
		"Continue anyway (fix it later; data stays spooled until then)",
	}, 0)
	if cerr != nil {
		return cerr
	}
	w.printf("\n")
	if c == 0 {
		return w.connectionSteps()
	}
	return nil
}

// offerPending offers the saved password of user at the configured endpoint.
func (w *wizard) offerPending(user string) (bool, error) {
	if w.pend == nil {
		return false, nil
	}
	e := w.pend.find(w.cfg.ClickHouse.URL, user)
	if e == nil {
		return false, nil
	}
	ok, err := w.p.confirm(fmt.Sprintf("Use the password saved when an earlier setup created %s (%s)?", user, config.DisplayPath(w.pendPath)), true)
	if err != nil || !ok {
		return false, err
	}
	w.pwAction, w.pwValue, w.pwGenerated = pwSet, e.Password, e.Generated
	w.printf("\n")
	return true, nil
}

// keepNewPassword writes the password of the user plan creates to the
// pending credentials file before anything is applied. If it cannot be
// written, the user is not created.
func (w *wizard) keepNewPassword(plan *clickhouse.Plan) error {
	if w.creating == "" || !hasStep(plan, clickhouse.StepUser) {
		return nil
	}
	if w.pendErr != nil {
		return fmt.Errorf("the new user's password cannot be kept safely: %w", w.pendErr)
	}
	e := pendingCredentials{Endpoint: w.cfg.ClickHouse.URL, Database: plan.Database, User: w.creating,
		Password: w.pwValue, Generated: w.pwGenerated, Created: time.Now().UTC()}
	if err := w.pend.put(e); err != nil {
		return fmt.Errorf("the new user's password cannot be kept safely in %s: %w", config.DisplayPath(w.pendPath), err)
	}
	k := pendKey(e.Endpoint, e.User)
	if w.newUsers[k] != "created" {
		w.newUsers[k] = "unknown"
	}
	return nil
}

// settleNewUser records the outcome of creating the user after a plan was
// applied. The saved password is removed only when the user was certainly
// not created.
func (w *wizard) settleNewUser(res clickhouse.ApplyResult) {
	if w.creating == "" || w.pend == nil {
		return
	}
	k := pendKey(w.cfg.ClickHouse.URL, w.creating)
	if _, ok := w.newUsers[k]; !ok {
		return
	}
	for _, s := range res.Applied {
		if s.Kind == clickhouse.StepUser {
			w.newUsers[k] = "created"
			return
		}
	}
	notCreated := false
	if res.Failed != nil && res.Failed.Kind == clickhouse.StepUser {
		ctx, cancel := context.WithTimeout(w.ctx, checkTimeout(w.cfg.ClickHouse))
		_, exists, err := w.admin.LookupUser(ctx, w.creating)
		cancel()
		notCreated = err == nil && !exists
	}
	for _, s := range res.NotAttempted {
		if s.Kind == clickhouse.StepUser && !w.pendFound[k] {
			notCreated = true
		}
	}
	if notCreated && w.newUsers[k] != "created" {
		if err := w.pend.drop(w.cfg.ClickHouse.URL, w.creating); err == nil {
			delete(w.newUsers, k)
		}
	}
}

func hasStep(p *clickhouse.Plan, kind clickhouse.StepKind) bool {
	for _, s := range p.Steps {
		if s.Kind == kind {
			return true
		}
	}
	return false
}

// settlePending removes the setup copy of the password the saved
// configuration now uses, and reminds about any other pending password.
func (w *wizard) settlePending() {
	if w.pend == nil {
		return
	}
	ch := w.cfg.ClickHouse
	if e := w.pend.find(ch.URL, ch.Username); e != nil && !w.pwFromEnv && storedPassword(w.cfg) == e.Password {
		if err := w.pend.drop(ch.URL, ch.Username); err != nil {
			w.printf("! The setup copy of %s's password could not be removed from %s: %v\n", ch.Username, config.DisplayPath(w.pendPath), err)
		} else {
			delete(w.newUsers, pendKey(ch.URL, ch.Username))
		}
	}
	for _, e := range w.pend.Users {
		w.printf("! The password of ClickHouse %s is still kept in %s:\n"+
			"  the saved configuration does not use it. Run `vario init` to finish a setup with it, or, if the\n"+
			"  user is not needed, have an administrator remove it (DROP USER %s) and delete the file.\n", e, config.DisplayPath(w.pendPath), e.User)
	}
}
