package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/nedanwr/vario/internal/config"
	"github.com/nedanwr/vario/internal/storage/clickhouse"
)

// ---- ClickHouse access ---------------------------------------------------------

func (w *wizard) connectionSteps() error {
	for {
		if err := w.stepEndpoint(); err != nil {
			return err
		}
		c, err := w.p.choose("How should Vario set up its ClickHouse access?", []string{
			"Guided setup — sign in with an administrative account (not saved) to choose or create the database and a restricted Vario user",
			"Manually managed — use an existing database and operational user; no administrative credentials needed",
		}, map[bool]int{true: 1, false: 0}[w.setup == "manual"])
		if err != nil {
			return err
		}
		w.printf("\n")
		if c == 0 {
			err = w.guidedSetup()
			if errors.Is(err, errManual) {
				err = w.manualSetup()
			}
		} else {
			err = w.manualSetup()
		}
		if errors.Is(err, errBack) {
			continue
		}
		return err
	}
}

func (w *wizard) stepEndpoint() error {
	w.printf("ClickHouse connection\n\n")
	def := w.cfg.ClickHouse.URL
	if def == "" {
		def = "http://localhost:8123"
	}
	for {
		u, err := w.p.askValid("ClickHouse endpoint (HTTP interface)", def, func(s string) error {
			if err := config.CheckURL(s); err != nil {
				return fmt.Errorf("the endpoint %v", err)
			}
			return nil
		})
		if err != nil {
			return err
		}
		if config.InsecureRemote(u) {
			w.printf("  Warning: plain http:// sends your data and credentials unencrypted over the network.\n")
			ok, err := w.p.confirm("  Use unencrypted HTTP for this remote server anyway?", false)
			if err != nil {
				return err
			}
			if !ok {
				def = "https://" + strings.TrimPrefix(u, "http://")
				continue
			}
			w.cfg.ClickHouse.AllowInsecureHTTP = true
		}
		if u != w.cfg.ClickHouse.URL {
			w.check, w.admin = checkNotRun, nil
		}
		w.cfg.ClickHouse.URL = u
		w.printf("\n")
		return nil
	}
}

// ---- guided setup -----------------------------------------------------------------

// userChoice is the operational user selected or to be created.
type userChoice struct {
	name     string
	create   bool
	user     clickhouse.User
	grants   *clickhouse.Plan // GRANT statements to apply
	assessed *clickhouse.GrantAssessment
}

func (w *wizard) guidedSetup() error {
	if w.admin != nil {
		ok, err := w.p.confirm(fmt.Sprintf("Continue as administrative user %s?", w.admin.User()), true)
		if err != nil {
			return err
		}
		if !ok {
			w.admin = nil
		}
		w.printf("\n")
	}
	if w.admin == nil {
		admin, _, err := promptAdmin(w.ctx, w.p, w.out, w.cfg.ClickHouse, adminOptions{user: w.adminUser}, true)
		if err != nil {
			return err
		}
		w.admin, w.adminUser = admin, admin.User()
	}
	w.setup = "guided"
	for {
		w.creating = ""
		db, exists, err := w.chooseDatabase()
		if err != nil {
			return err
		}
		uc, err := w.chooseUser(db, exists)
		if errors.Is(err, errBack) {
			continue
		}
		if err != nil {
			return err
		}
		schema, err := w.planSchema(db)
		if errors.Is(err, errBack) {
			continue
		}
		if err != nil {
			return err
		}
		plan := &clickhouse.Plan{Database: db}
		if !exists {
			plan.Steps = append(plan.Steps, clickhouse.CreateDatabaseStep(db))
		}
		plan.Append(schema)
		if uc.create {
			step, err := clickhouse.CreateUserStep(uc.name, w.pwValue, db)
			if err != nil {
				return err
			}
			plan.Steps = append(plan.Steps, step)
			w.creating = uc.name
		}
		plan.Append(uc.grants)
		back, err := w.confirmAndApply(plan)
		if err != nil {
			return err
		}
		if back {
			continue
		}
		w.cfg.ClickHouse.Database, w.cfg.ClickHouse.Username = db, uc.name
		w.recordStates(db, exists, uc, schema)
		if back, err = w.verifyOperational(); err != nil {
			return err
		}
		if back {
			continue
		}
		return nil
	}
}

func (w *wizard) recordStates(db string, existed bool, uc userChoice, schema *clickhouse.Plan) {
	switch {
	case !existed:
		w.dbState = "created by this setup"
	default:
		w.dbState = "existing database"
	}
	switch {
	case uc.create:
		w.userState = "dedicated user created by this setup"
	case uc.name == w.adminUser:
		w.userState = "existing user (also the administrative account)"
	default:
		w.userState = "existing user"
	}
	switch {
	case uc.grants.Empty():
		w.privState = "unchanged"
	default:
		w.privState = fmt.Sprintf("granted by this setup (%d objects, scoped to Vario's tables and views)", len(uc.grants.Steps))
	}
	if schema.Empty() {
		w.schemaState = ""
	} else {
		w.schemaState = fmt.Sprintf("created or migrated by this setup (version %d)", clickhouse.SchemaVersion)
	}
}

// chooseDatabase selects an existing database or names a new one. exists
// reports whether it already exists.
func (w *wizard) chooseDatabase() (db string, exists bool, err error) {
	w.printf("Database\n\n")
	c, err := w.p.choose("", []string{"Use an existing database", "Create a new database", "Go back"}, 0)
	if err != nil {
		return "", false, err
	}
	w.printf("\n")
	switch c {
	case 2:
		return "", false, errBack
	case 1:
		return w.newDatabase()
	}
	ctx, cancel := context.WithTimeout(w.ctx, checkTimeout(w.cfg.ClickHouse))
	names, lerr := w.admin.ListDatabases(ctx)
	cancel()
	if lerr != nil {
		w.printf("  Databases cannot be listed (%s: %v).\n  Enter the name instead.\n\n", clickhouse.Classify(lerr), lerr)
		return w.manualDatabase()
	}
	showSystem, filter := false, ""
	for {
		shown := clickhouse.FilterDatabases(names, showSystem, filter)
		var usable []string
		unsupported := 0
		for _, n := range shown {
			if config.CheckDatabaseName(n) == nil {
				usable = append(usable, n)
			} else {
				unsupported++
			}
		}
		if len(usable) > 25 && filter == "" {
			if filter, err = w.p.ask(fmt.Sprintf("%d databases are visible. Filter by name (part of the name, empty for all)", len(usable)), ""); err != nil {
				return "", false, err
			}
			if filter != "" {
				continue
			}
		}
		var opts []string
		for _, n := range usable {
			label := n
			if n == w.cfg.ClickHouse.Database && w.orig != nil {
				label += "  (currently configured)"
			}
			if clickhouse.IsSystemDatabase(n) {
				label += "  (system database; not recommended)"
			}
			opts = append(opts, label)
		}
		base := len(opts)
		opts = append(opts, "Enter a name manually")
		if showSystem {
			opts = append(opts, "Hide system databases")
		} else {
			opts = append(opts, "Show system databases (system, default, ...)")
		}
		opts = append(opts, "Filter the list", "Go back")
		heading := fmt.Sprintf("Databases visible to %s:", w.admin.User())
		if len(usable) == 0 {
			heading = fmt.Sprintf("No databases besides the system databases are visible to %s.", w.admin.User())
		}
		if filter != "" {
			heading += fmt.Sprintf(" (filter: %q)", filter)
		}
		def := base
		for i, n := range usable {
			if n == w.cfg.ClickHouse.Database {
				def = i
			}
		}
		pick, err := w.p.choose(heading, opts, def)
		if err != nil {
			return "", false, err
		}
		if unsupported > 0 && pick == base {
			w.printf("  (%d databases with names Vario does not support are not listed.)\n", unsupported)
		}
		w.printf("\n")
		switch {
		case pick < base:
			name := usable[pick]
			if clickhouse.IsSystemDatabase(name) {
				ok, err := w.p.confirm(fmt.Sprintf("%s is a system or default database. A dedicated database is recommended. Use it anyway?", name), false)
				if err != nil {
					return "", false, err
				}
				w.printf("\n")
				if !ok {
					continue
				}
			}
			return name, true, nil
		case pick == base:
			return w.manualDatabase()
		case pick == base+1:
			showSystem = !showSystem
		case pick == base+2:
			if filter, err = w.p.ask("Filter by name (part of the name, empty for all)", filter); err != nil {
				return "", false, err
			}
			w.printf("\n")
		default:
			return w.chooseDatabase()
		}
	}
}

func (w *wizard) askDatabaseName(label, def string) (string, error) {
	return w.p.askValid(label, def, func(s string) error {
		if err := config.CheckDatabaseName(s); err != nil {
			return fmt.Errorf("the database name %v", err)
		}
		return nil
	})
}

// manualDatabase asks for the name of an existing database. Databases the
// administrator cannot see are not assumed to be missing; the user may
// create the database instead.
func (w *wizard) manualDatabase() (string, bool, error) {
	for {
		name, err := w.askDatabaseName("Database name", w.cfg.ClickHouse.Database)
		if err != nil {
			return "", false, err
		}
		exists, err := w.databaseExists(name)
		if err != nil {
			w.printf("  Whether %s exists cannot be checked (%v); continuing with it.\n\n", name, err)
			return name, true, nil
		}
		if exists {
			w.printf("\n")
			return name, true, nil
		}
		c, err := w.p.choose(fmt.Sprintf("\nDatabase %s does not exist (or is not visible to %s).", name, w.admin.User()), []string{
			"Create it", "Enter another name", "Go back"}, 0)
		if err != nil {
			return "", false, err
		}
		w.printf("\n")
		switch c {
		case 0:
			return name, false, nil
		case 2:
			return w.chooseDatabase()
		}
	}
}

func (w *wizard) newDatabase() (string, bool, error) {
	def := w.cfg.ClickHouse.Database
	if def == "" {
		def = "vario"
	}
	for {
		name, err := w.askDatabaseName("Name of the new database", def)
		if err != nil {
			return "", false, err
		}
		exists, err := w.databaseExists(name)
		if err != nil {
			w.printf("  Whether %s exists cannot be checked (%v).\n", name, err)
		}
		if !exists {
			w.printf("  Database %s will be created after you review all changes.\n\n", name)
			return name, false, nil
		}
		c, err := w.p.choose(fmt.Sprintf("\nDatabase %s already exists. Vario never drops or recreates databases.", name), []string{
			"Use the existing database", "Choose another name"}, 0)
		if err != nil {
			return "", false, err
		}
		w.printf("\n")
		if c == 0 {
			return name, true, nil
		}
		def = ""
	}
}

func (w *wizard) databaseExists(name string) (bool, error) {
	ctx, cancel := context.WithTimeout(w.ctx, checkTimeout(w.cfg.ClickHouse))
	defer cancel()
	return w.admin.ForDatabase(name).DatabaseExists(ctx)
}

// chooseUser selects the operational user. New databases default to a
// dedicated user; existing users are inspected before their password is asked.
func (w *wizard) chooseUser(db string, dbExists bool) (userChoice, error) {
	w.printf("Operational user (used by the collector and the background service)\n\n")
	def := 0
	if dbExists && w.orig != nil && w.cfg.ClickHouse.Username != "" && w.cfg.ClickHouse.Username != "default" {
		def = 1
	}
	c, err := w.p.choose("", []string{
		"Create a dedicated Vario user (recommended; only the privileges Vario needs)",
		"Use an existing ClickHouse user",
		"Go back",
	}, def)
	if err != nil {
		return userChoice{}, err
	}
	w.printf("\n")
	switch c {
	case 0:
		return w.newUser(db)
	case 1:
		return w.existingUser(db, "")
	}
	return userChoice{}, errBack
}

func (w *wizard) newUser(db string) (userChoice, error) {
	def := "vario"
	for {
		name, err := w.p.askValid("Name of the new user", def, func(s string) error {
			if err := clickhouse.CheckUserName(s); err != nil {
				return fmt.Errorf("the user name %v", err)
			}
			return nil
		})
		if err != nil {
			return userChoice{}, err
		}
		ctx, cancel := context.WithTimeout(w.ctx, checkTimeout(w.cfg.ClickHouse))
		u, exists, lerr := w.admin.LookupUser(ctx, name)
		cancel()
		switch {
		case lerr != nil:
			w.printf("  Whether user %s exists cannot be checked (%v). If it does, creating it fails and nothing is changed.\n", name, lerr)
		case exists:
			c, err := w.p.choose(fmt.Sprintf("\nUser %s already exists. Vario does not change existing users' credentials.", name), []string{
				"Use the existing user " + name, "Choose another name", "Go back"}, 0)
			if err != nil {
				return userChoice{}, err
			}
			w.printf("\n")
			switch c {
			case 0:
				return w.existingUser(db, u.Name)
			case 1:
				def = ""
				continue
			}
			return userChoice{}, errBack
		}
		if err := w.newPassword(name); err != nil {
			return userChoice{}, err
		}
		return userChoice{name: name, create: true, grants: clickhouse.GrantPlan(name, db, clickhouse.OperationalRequirements())}, nil
	}
}

// newPassword asks for the new user's password, or generates one.
func (w *wizard) newPassword(user string) error {
	if ok, err := w.offerPending(user); err != nil || ok {
		return err
	}
	for {
		pw, err := w.p.secret(fmt.Sprintf("Password for %s (input hidden; leave empty to generate a strong one)", user))
		if err != nil {
			return err
		}
		if pw == "" {
			if pw, err = clickhouse.GeneratePassword(); err != nil {
				return err
			}
			w.pwAction, w.pwValue, w.pwGenerated = pwSet, pw, true
			w.printf("  A random 32-character password was generated. It is saved for Vario in %s\n  (readable only by you) when you save the configuration, and never printed in logs.\n",
				config.DisplayPath(w.cfg.PasswordPath()))
			show, err := w.p.confirm("  Show it once now, for example to keep it in a password manager?", false)
			if err != nil {
				return err
			}
			if show {
				w.printf("\n    %s\n\n  It will not be shown again.\n", pw)
			}
			w.printf("\n")
			return nil
		}
		again, err := w.p.secret("Repeat the password")
		if err != nil {
			return err
		}
		if again != pw {
			w.printf("  The passwords do not match.\n")
			continue
		}
		w.pwAction, w.pwValue, w.pwGenerated = pwSet, pw, false
		w.printf("\n")
		return nil
	}
}

func (w *wizard) existingUser(db, preset string) (userChoice, error) {
	name := preset
	var u clickhouse.User
	known := false
	for name == "" {
		ctx, cancel := context.WithTimeout(w.ctx, checkTimeout(w.cfg.ClickHouse))
		users, lerr := w.admin.ListUsers(ctx)
		cancel()
		if lerr != nil {
			w.printf("  Users cannot be listed (%s: %v).\n", clickhouse.Classify(lerr), lerr)
			n, err := w.askUserName()
			if err != nil {
				return userChoice{}, err
			}
			name = n
			break
		}
		var opts []string
		def := len(users)
		for i, us := range users {
			label := us.Name
			if note := us.StorageNote(); note != "" {
				label += "  (" + note + ")"
			}
			if us.Name == w.admin.User() {
				label += "  (the administrative account; not recommended)"
			}
			if us.Name == w.cfg.ClickHouse.Username {
				def = i
			}
			opts = append(opts, label)
		}
		opts = append(opts, "Enter a user name manually", "Go back")
		pick, err := w.p.choose(fmt.Sprintf("Users visible to %s:", w.admin.User()), opts, def)
		if err != nil {
			return userChoice{}, err
		}
		w.printf("\n")
		switch {
		case pick < len(users):
			u, name, known = users[pick], users[pick].Name, true
		case pick == len(users):
			n, err := w.askUserName()
			if err != nil {
				return userChoice{}, err
			}
			name = n
		default:
			return userChoice{}, errBack
		}
	}
	if !known {
		ctx, cancel := context.WithTimeout(w.ctx, checkTimeout(w.cfg.ClickHouse))
		found, ok, err := w.admin.LookupUser(ctx, name)
		cancel()
		switch {
		case err == nil && !ok:
			w.printf("  User %s does not exist (or is not visible to %s).\n\n", name, w.admin.User())
			return w.chooseUser(db, true)
		case err == nil:
			u, known = found, true
		}
	}
	if name == w.admin.User() {
		w.printf("! %s is the administrative account you signed in with. Its password would be stored for the\n  background collector, which then runs with administrative privileges. A dedicated user is recommended.\n", name)
		ok, err := w.p.confirm("Use it anyway?", false)
		if err != nil {
			return userChoice{}, err
		}
		w.printf("\n")
		if !ok {
			return w.chooseUser(db, true)
		}
	}
	uc := userChoice{name: name, user: u}

	// Inspect grants before asking for the user's password.
	ctx, cancel := context.WithTimeout(w.ctx, checkTimeout(w.cfg.ClickHouse))
	as, err := w.admin.AssessGrants(ctx, name, db)
	cancel()
	if err != nil {
		as = clickhouse.GrantAssessment{Status: clickhouse.GrantsInconclusive, Notes: []string{err.Error()}}
	}
	uc.assessed = &as
	w.printf("Privileges of %s\n", name)
	printAssessment(w.out, name, db, as)
	w.printf("\n")
	for as.Status != clickhouse.GrantsSufficient {
		modifiable := !known || u.Modifiable()
		opts := []string{}
		actions := []string{}
		if len(as.Missing) > 0 && modifiable {
			opts = append(opts, fmt.Sprintf("Authorize Vario to grant the missing privileges to %s (reviewed before anything is changed)", name))
			actions = append(actions, "grant")
		}
		opts = append(opts, "Show the required SQL", "Choose another user", "Continue without changing privileges (the login check verifies access)")
		actions = append(actions, "sql", "other", "continue")
		if known && !u.Modifiable() {
			w.printf("  %s is %s.\n", name, u.StorageNote())
		}
		c, err := w.p.choose("What would you like to do?", opts, 0)
		if err != nil {
			return userChoice{}, err
		}
		w.printf("\n")
		switch actions[c] {
		case "grant":
			uc.grants = clickhouse.GrantPlan(name, db, as.Missing)
		case "sql":
			w.printf("Statements an administrator can run:\n\n%s\n", indent(requiredSQL(db, name, false, as.Missing), "  "))
			continue
		case "other":
			return w.existingUser(db, "")
		}
		break
	}
	if err := w.existingPassword(name); err != nil {
		return userChoice{}, err
	}
	return uc, nil
}

func (w *wizard) askUserName() (string, error) {
	return w.p.askValid("User name", w.cfg.ClickHouse.Username, func(s string) error {
		if s == "" {
			return errors.New("a user name is required")
		}
		return nil
	})
}

// existingPassword asks for the password of an existing user, offering to
// keep a stored one for the same user. Vario never changes it in ClickHouse.
func (w *wizard) existingPassword(user string) error {
	w.pwGenerated = false
	if w.pwFromEnv {
		w.printf("Using the password from $%s; it is not saved.\n\n", config.EnvPassword)
		return nil
	}
	if ok, err := w.offerPending(user); err != nil || ok {
		return err
	}
	if w.pwStored != "" && w.orig != nil && w.orig.ClickHouse.Username == user {
		keep, err := w.p.confirm(fmt.Sprintf("Keep the password stored for %s in %s?", user, config.DisplayPath(w.pwStored)), true)
		if err != nil {
			return err
		}
		if keep {
			w.pwAction, w.pwValue = pwKeep, ""
			w.printf("\n")
			return nil
		}
	}
	pw, err := w.p.secret(fmt.Sprintf("Password for %s (input hidden; leave empty for none)", user))
	if err != nil {
		return err
	}
	switch {
	case pw != "":
		w.pwAction, w.pwValue = pwSet, pw
	case w.pwStored != "":
		w.pwAction, w.pwValue = pwClear, ""
	default:
		w.pwAction, w.pwValue = pwKeep, ""
	}
	w.printf("\n")
	return nil
}

// planSchema returns the schema steps for db, or nil when the user prefers
// to provision the schema separately.
func (w *wizard) planSchema(db string) (*clickhouse.Plan, error) {
	ctx, cancel := context.WithTimeout(w.ctx, checkTimeout(w.cfg.ClickHouse))
	plan, err := w.admin.ForDatabase(db).PlanSchema(ctx)
	cancel()
	if err != nil {
		var se *clickhouse.SchemaError
		if errors.As(err, &se) || errors.Is(err, clickhouse.ErrSchemaNewer) {
			w.printf("✗ %v\n  %s\n", err, clickhouse.Hint(clickhouse.Classify(err)))
			c, cerr := w.p.choose("", []string{"Choose another database", "Cancel setup"}, 0)
			if cerr != nil {
				return nil, cerr
			}
			w.printf("\n")
			if c == 0 {
				return nil, errBack
			}
			return nil, errCancelled
		}
		w.printf("! The schema in %s cannot be inspected (%v). Create it later with `vario migrate`.\n\n", db, err)
		return nil, nil
	}
	steps := &clickhouse.Plan{Database: db}
	for _, s := range plan.Steps {
		if s.Kind != clickhouse.StepDatabase {
			steps.Steps = append(steps.Steps, s)
		}
	}
	w.printf("Vario schema in %s: %s\n", db, describeVersion(plan.Status))
	if steps.Empty() {
		w.printf("  ✓ up to date; nothing to create\n\n")
		return nil, nil
	}
	ok, err := w.p.confirm(fmt.Sprintf("Create or update Vario's schema (%d steps) with the administrative account now? (recommended)", len(steps.Steps)), true)
	if err != nil {
		return nil, err
	}
	w.printf("\n")
	if !ok {
		w.printf("  The schema can be created later with `vario migrate`. Until then, data stays spooled.\n\n")
		return nil, nil
	}
	return steps, nil
}

// confirmAndApply shows the provisioning plan, applies it after explicit
// confirmation and handles partial failures. back is set when the user
// wants to change the selections.
func (w *wizard) confirmAndApply(plan *clickhouse.Plan) (back bool, err error) {
	if plan.Empty() {
		return false, nil
	}
	for {
		w.printf("Planned changes in ClickHouse (%s):\n\n", w.cfg.ClickHouse.URL)
		printPlan(w.out, plan, false)
		w.printf("\nNothing has been changed yet. Existing databases, tables and users are never dropped or recreated.\n")
		c, err := w.p.choose("", []string{"Apply these changes", "Show the SQL statements", "Change the selections", "Cancel setup (nothing is changed)"}, 0)
		if err != nil {
			return false, err
		}
		w.printf("\n")
		switch c {
		case 1:
			printPlan(w.out, plan, true)
			w.printf("\n")
			continue
		case 2:
			return true, nil
		case 3:
			return false, errCancelled
		}
		for {
			// The new user's password is on disk before the user exists.
			if err := w.keepNewPassword(plan); err != nil {
				w.printf("✗ %v\n  The remaining changes were not applied.\n", err)
				return false, withCode(ExitFailure, err)
			}
			w.applying = true
			w.updateNotice()
			w.printf("Applying:\n")
			res := applyPlan(w.ctx, w.out, w.admin, plan)
			w.applying = false
			if len(res.Applied) > 0 {
				w.changedCH = true
			}
			w.settleNewUser(res)
			w.updateNotice()
			if res.OK() {
				for _, wn := range res.Warnings {
					w.printf("  ! %s\n", wn)
				}
				w.printf("\n")
				return false, nil
			}
			c, err := w.p.choose("\nWhat would you like to do?", []string{
				"Retry the remaining steps",
				"Change the selections (applied steps remain)",
				"Quit (applied steps remain; nothing is saved locally)",
			}, 0)
			if err != nil {
				return false, err
			}
			w.printf("\n")
			switch c {
			case 0:
				plan = remaining(plan, res)
			case 1:
				return true, nil
			default:
				return false, errCancelled
			}
		}
	}
}

// verifyOperational checks the operational login independently of the
// administrative inspection. back is set to change the selections.
func (w *wizard) verifyOperational() (back bool, err error) {
	for {
		w.printf("Verifying the operational login as %s:\n", w.cfg.ClickHouse.Username)
		rep, err := w.runCheck()
		if err == nil && rep.Ready() {
			w.printf("\n")
			return false, nil
		}
		c, cerr := w.p.choose("\nThe operational login is not ready. What would you like to do?", []string{
			"Check again",
			"Change the database or user",
			"Continue anyway (fix it later; data stays spooled until then)",
		}, 0)
		if cerr != nil {
			return false, cerr
		}
		w.printf("\n")
		switch c {
		case 1:
			return true, nil
		case 2:
			return false, nil
		}
	}
}

// ---- manually managed setup ---------------------------------------------------------

func (w *wizard) manualSetup() error {
	w.setup = "manual"
	w.dbState, w.userState, w.privState, w.schemaState = "existing database (managed manually)", "existing user (managed manually)", "", ""
	for {
		db, err := w.askDatabaseName("Database name", w.cfg.ClickHouse.Database)
		if err != nil {
			return err
		}
		w.cfg.ClickHouse.Database = db
		user, err := w.p.askValid("Operational ClickHouse user", w.cfg.ClickHouse.Username, func(s string) error {
			if s == "" {
				return errors.New("a username is required")
			}
			return nil
		})
		if err != nil {
			return err
		}
		if user != w.cfg.ClickHouse.Username {
			w.check = checkNotRun
		}
		w.cfg.ClickHouse.Username = user
		if err := w.stepPassword(); err != nil {
			return err
		}
		ok, err := w.p.confirm("Verify the connection now? (read-only; nothing is changed)", true)
		if err != nil {
			return err
		}
		if !ok {
			w.check, w.checkNote, w.report = checkNotRun, "not tested", nil
			w.printf("\n")
			return nil
		}
		for {
			w.printf("\n")
			rep, err := w.runCheck()
			if err == nil && rep.Ready() {
				w.printf("\n")
				return nil
			}
			opts := []string{"Check again", "Show the SQL an administrator needs to run", "Change the settings", "Use guided setup instead", "Continue without verifying (the settings are kept and can be saved)"}
			c, cerr := w.p.choose("\nThe connection is not ready. What would you like to do?", opts, 0)
			if cerr != nil {
				return cerr
			}
			w.printf("\n")
			switch c {
			case 0:
				continue
			case 1:
				w.printf("An administrator can provision Vario with:\n\n%s\n  Then create the schema: `vario migrate` (preview its SQL with `vario migrate --dry-run`).\n",
					indent(requiredSQL(db, user, true, clickhouse.OperationalRequirements()), "  "))
				continue
			case 2:
			case 3:
				return w.guidedSetup()
			case 4:
				return nil
			}
			break
		}
	}
}

// stepPassword asks for the operational password, offering to keep a stored one.
func (w *wizard) stepPassword() error {
	w.pwGenerated = false
	if w.pwFromEnv {
		w.printf("Using the password from $%s for this setup; it is not saved.\n\n", config.EnvPassword)
		return nil
	}
	if w.pwStored != "" || w.pwAction == pwSet {
		where := "is already stored in " + config.DisplayPath(w.pwStored)
		if w.pwAction == pwSet {
			where = "was entered"
		}
		c, err := w.p.choose("A ClickHouse password "+where+".", []string{"Keep it", "Enter a different password", "Use no password"}, 0)
		if err != nil {
			return err
		}
		switch c {
		case 0:
			w.printf("\n")
			return nil
		case 2:
			w.pwAction, w.pwValue = pwClear, ""
			w.printf("\n")
			return nil
		}
	}
	pw, err := w.p.secret("Password (input hidden; leave empty for none)")
	if err != nil {
		return err
	}
	switch {
	case pw != "":
		w.pwAction, w.pwValue = pwSet, pw
	case w.pwStored != "":
		w.pwAction, w.pwValue = pwClear, ""
	default:
		w.pwAction, w.pwValue = pwKeep, ""
	}
	w.printf("\n")
	return nil
}

// ---- operational check ------------------------------------------------------------------

// password returns the operational password to use for checks before it is saved.
func (w *wizard) password() (string, error) {
	if p, ok := os.LookupEnv(config.EnvPassword); ok {
		return p, nil
	}
	switch w.pwAction {
	case pwSet:
		return w.pwValue, nil
	case pwClear:
		return "", nil
	}
	if w.pwStored == "" {
		return "", nil
	}
	b, err := os.ReadFile(w.pwStored)
	if err != nil {
		return "", fmt.Errorf("read password file: %w", err)
	}
	return strings.TrimRight(string(b), "\r\n"), nil
}

func (w *wizard) client() (*clickhouse.Client, error) {
	pw, err := w.password()
	if err != nil {
		return nil, err
	}
	return clickhouse.New(w.cfg.ClickHouse, pw)
}

// runCheck verifies the operational login (read-only) and prints the findings.
func (w *wizard) runCheck() (*clickhouse.Report, error) {
	w.report = nil
	client, err := w.client()
	if err != nil {
		w.check, w.checkNote = checkFailed, err.Error()
		w.printf("  ✗ %v\n", err)
		return nil, err
	}
	ctx, cancel := context.WithTimeout(w.ctx, checkTimeout(w.cfg.ClickHouse))
	defer cancel()
	rep, err := client.CheckConnection(ctx)
	if err != nil {
		var ce *clickhouse.CheckError
		kind := clickhouse.Classify(err)
		if errors.As(err, &ce) {
			kind = ce.Kind
			if ce.Stage != "reach" {
				w.printf("  ✓ endpoint reachable (%s)\n", w.cfg.ClickHouse.URL)
			}
		}
		w.check, w.checkNote = checkFailed, fmt.Sprintf("%s failure", kind)
		w.printf("  ✗ %s failure: %v\n    %s\n", kind, err, clickhouse.Hint(kind))
		return nil, err
	}
	w.report = rep
	printReport(w.out, rep)
	switch {
	case rep.Verified():
		w.check, w.checkNote = checkVerified, fmt.Sprintf("verified as %s (ClickHouse %s)", rep.User, rep.Version)
	case rep.Ready():
		w.check, w.checkNote = checkPartial, fmt.Sprintf("connected as %s; not every check was conclusive", rep.User)
	default:
		w.check, w.checkNote = checkVerified, "connected, with problems (see above)"
	}
	return rep, nil
}

func indent(s, prefix string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, l := range lines {
		lines[i] = prefix + l
	}
	return strings.Join(lines, "\n") + "\n"
}

// ---- summary and saving -------------------------------------------------------------------

func (w *wizard) passwordSummary() string {
	switch {
	case w.pwFromEnv:
		return "from $" + config.EnvPassword + " (not saved)"
	case w.pwAction == pwSet && w.pwGenerated:
		return "generated, saved to " + config.DisplayPath(w.cfg.PasswordPath()) + " (not shown)"
	case w.pwAction == pwSet:
		return "new password, saved to " + config.DisplayPath(w.cfg.PasswordPath())
	case w.pwAction == pwClear && w.pwStored != "":
		return "none (the stored password will be removed)"
	case w.pwStored != "":
		return "stored in " + config.DisplayPath(w.pwStored) + " (unchanged)"
	}
	return "none"
}

func (w *wizard) schemaSummary() string {
	if w.schemaState != "" && w.report == nil {
		return w.schemaState
	}
	if w.report == nil {
		return "not checked (`vario migrate` creates or updates it)"
	}
	st := w.report.Schema
	line := strings.TrimSpace(strings.TrimLeft(schemaLine(st), "✓✗!·"))
	if !st.DatabaseExists {
		line = "unknown (database not accessible)"
	}
	if w.schemaState != "" {
		line += "; " + w.schemaState
	}
	return line
}

func (w *wizard) summary() {
	c := w.cfg
	if w.check == checkNotRun && w.checkNote == "" {
		w.checkNote = "not tested"
	}
	srcs := "none"
	var parts []string
	for _, s := range c.Sources {
		if s.Enabled {
			parts = append(parts, fmt.Sprintf("%s (%s)", s.Kind, config.DisplayPath(sourcePath(s))))
		}
	}
	if len(parts) > 0 {
		srcs = strings.Join(parts, ", ")
	}
	setup := "manually managed (no administrative credentials)"
	switch {
	case w.setup == "guided":
		setup = fmt.Sprintf("guided, as administrative user %s (credentials not saved)", w.adminUser)
	case w.setup == "resumed":
		setup = "finishing an earlier guided setup"
	}
	db := c.ClickHouse.Database
	if w.dbState != "" {
		db += " — " + w.dbState
	}
	user := c.ClickHouse.Username
	if w.userState != "" {
		user += " — " + w.userState
	}
	w.printf("Summary\n\n")
	rows := [][2]string{
		{"ClickHouse endpoint", c.ClickHouse.URL},
		{"Setup", setup},
		{"Database", db},
		{"Operational user", user},
		{"Password", w.passwordSummary()},
	}
	if w.privState != "" {
		rows = append(rows, [2]string{"Privileges", w.privState})
	}
	rows = append(rows,
		[2]string{"Schema", w.schemaSummary()},
		[2]string{"Connection", w.checkNote},
		[2]string{"Collection mode", c.Privacy.Mode},
		[2]string{"Sources", srcs},
		[2]string{"Configuration file", config.DisplayPath(w.path)},
	)
	for _, r := range rows {
		w.printf("  %-20s %s\n", r[0], r[1])
	}
	if w.changedCH {
		w.printf("\n  Changes in ClickHouse were applied when you confirmed them; saving writes only local files.\n")
	}
	w.printf("\n")
}

// confirmQuit asks before quitting when a user created by this setup is not
// used by a saved configuration yet. Its password stays in the pending
// credentials file either way.
func (w *wizard) confirmQuit() (bool, error) {
	if users := w.unsavedUsers(); len(users) > 0 {
		_, name := splitKey(users[0])
		w.printf("User %s was created by this setup, and the configuration that uses it is not saved yet.\n", name)
		c, err := w.p.choose("", []string{
			"Go back to the summary",
			fmt.Sprintf("Quit without saving (the password stays in %s so that `vario init` can finish later)", config.DisplayPath(w.pendPath)),
		}, 0)
		if err != nil {
			return false, err
		}
		w.printf("\n")
		if c == 0 {
			return false, nil
		}
	}
	w.unsavedNotice(true)
	if w.changedCH || len(w.unsavedUsers()) > 0 {
		w.printf("Nothing was saved locally.\n")
	}
	return true, nil
}

func (w *wizard) saveAndFinish() error {
	saved, err := w.save()
	if err != nil {
		return err
	}
	w.settlePending()
	w.p.setNotice("Setup interrupted after the configuration was saved.\n")
	if saved {
		w.printf("✓ Configuration saved\n  %s\n", config.DisplayPath(w.path))
	} else {
		w.printf("✓ No configuration changes; %s was left untouched\n", config.DisplayPath(w.path))
	}
	if w.pwAction == pwSet {
		w.printf("✓ Operational password saved to %s (readable only by you)\n", config.DisplayPath(w.cfg.PasswordPath()))
	} else if w.pwAction == pwClear && w.pwStored != "" {
		w.printf("✓ Stored password removed\n")
	}
	if w.admin != nil {
		w.printf("✓ Administrative credentials were used only during setup and were not saved\n")
		w.admin = nil
	}
	switch w.check {
	case checkVerified:
	case checkPartial:
		w.printf("! The operational login is not fully verified (%s). Check it later with `vario doctor`.\n", w.checkNote)
	default:
		w.printf("! The operational login is not verified (%s). Check it later with `vario doctor`.\n", w.checkNote)
	}
	for _, l := range legacyWarnings(w.cfg) {
		w.printf("! %s\n", l)
	}
	destinationNotice(w.out, w.prev, w.cfg)
	serviceActive := w.a.serviceNotice(w.path, saved || w.pwAction != pwKeep)
	w.printf("\nNext steps:\n\n")
	if w.report == nil || !w.report.Schema.Complete() {
		w.printf("  vario migrate                    # create or update Vario's schema (administrative credentials)\n")
	}
	w.printf("  vario doctor\n")
	if _, ok := w.cfg.Source(config.SourceT3Code); ok {
		w.printf("  vario backfill --source t3code\n")
	} else {
		w.printf("  vario sources detect\n")
	}
	w.printf("  vario run\n")
	if !serviceActive {
		w.printf("\nTo run in the background:\n  vario service install\n")
	}
	return nil
}
