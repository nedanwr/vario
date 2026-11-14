package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/nedanwr/vario/internal/config"
	"github.com/nedanwr/vario/internal/storage/clickhouse"
)

type migrateOptions struct {
	dryRun         bool
	yes            bool
	nonInteractive bool
	admin          adminOptions
}

func (a *app) migrateCmd() *cobra.Command {
	var o migrateOptions
	cmd := &cobra.Command{
		Use:   "migrate",
		Short: "Create or update Vario's ClickHouse schema (needs administrative credentials)",
		Long: fmt.Sprintf(`Migrate inspects Vario's schema in the configured database, shows what has to
change, and applies it after explicit confirmation, using administrative
credentials that are asked for when needed and never saved. It is the only
command that changes Vario's schema; the collector never does.

Migrations are additive and safe to repeat: missing tables, views and
metadata are created, Vario's own outdated views are replaced, and the schema
version (currently %d) and a destination id are recorded in vario_meta.
Databases and tables are never dropped, truncated or altered, and objects
Vario did not create are never touched: incompatible objects stop the
migration. ClickHouse DDL is not transactional, so a failed migration can be
partially applied; it is reported as such, and running migrate again resumes.

If the configured database is missing, migrate offers to create it. When the
operational user (clickhouse.username) is a different, SQL-managed user,
migrate also offers to grant it the privileges for new objects; existing
users are never changed without a separate confirmation.

  vario migrate --dry-run     show the planned changes and their SQL, change nothing
  vario migrate               apply after confirmation
  vario migrate --non-interactive --yes --admin-user default --admin-password-stdin < pw`, clickhouse.SchemaVersion),
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.runMigrate(cmd.Context(), o)
		},
	}
	f := cmd.Flags()
	f.BoolVar(&o.dryRun, "dry-run", false, "show the planned changes and their SQL without changing anything")
	f.BoolVar(&o.yes, "yes", false, "apply without asking for confirmation (required with --non-interactive)")
	f.BoolVar(&o.nonInteractive, "non-interactive", false, "do not prompt")
	f.StringVar(&o.admin.user, "admin-user", "", "administrative ClickHouse user (default: asked; \"default\")")
	f.BoolVar(&o.admin.passwordStdin, "admin-password-stdin", false, "read the administrative password from stdin")
	return cmd
}

func (a *app) migratePrompter(nonInteractive bool) *prompter {
	interactive := !nonInteractive
	if _, ok := a.stdin.(*os.File); ok && interactive && !isTerminal(a.stdin) {
		interactive = false
	}
	return newPrompter(a.stdin, a.stdout, interactive)
}

func (a *app) runMigrate(ctx context.Context, o migrateOptions) error {
	cfg, err := a.loadConfig()
	if err != nil {
		return err
	}
	p := a.migratePrompter(o.nonInteractive)
	return migrate(ctx, a.stdout, p, cfg, o)
}

func migrate(ctx context.Context, out io.Writer, p *prompter, cfg *config.Config, o migrateOptions) error {
	cc := cfg.ClickHouse
	fmt.Fprintf(out, "Vario schema migration\n  endpoint  %s\n  database  %s\n  collector user  %s\n\n", cc.URL, cc.Database, cc.Username)

	// 1. A read-only preview with the operational user: no administrative
	// credentials are needed when nothing has to change.
	op, err := newClient(cfg)
	if err != nil {
		return withCode(ExitUsage, err)
	}
	octx, cancel := context.WithTimeout(ctx, checkTimeout(cc))
	plan, perr := op.PlanSchema(octx)
	cancel()
	fatal := func(err error) bool {
		var se *clickhouse.SchemaError
		return errors.As(err, &se) || errors.Is(err, clickhouse.ErrSchemaNewer)
	}
	switch {
	case perr == nil && plan.Empty():
		fmt.Fprintf(out, "✓ Vario schema in %s is up to date (version %d). Nothing to do.\n", cc.Database, clickhouse.SchemaVersion)
		return nil
	case perr != nil && fatal(perr):
		fmt.Fprintf(out, "✗ %v\n  %s\n", perr, clickhouse.Hint(clickhouse.Classify(perr)))
		return withCode(ExitFailure, errors.New("migration not possible"))
	case perr != nil:
		fmt.Fprintf(out, "! Inspection as %s failed (%s): %v\n", cc.Username, clickhouse.Classify(perr), perr)
	default:
		fmt.Fprintf(out, "Current schema (as visible to %s): %s\n", cc.Username, describeVersion(plan.Status))
	}

	if o.dryRun && o.admin.user == "" {
		if perr != nil {
			fmt.Fprintf(out, "\nDry run: the schema could not be inspected, so no plan is shown. Fix the connection, or pass --admin-user to inspect with administrative credentials.\n")
			return withCode(ExitFailure, errors.New("dry run incomplete"))
		}
		fmt.Fprintf(out, "\nPlanned changes (based on what %s can see; an administrator may see more):\n", cc.Username)
		printPlan(out, plan, true)
		fmt.Fprintln(out, "\nDry run: nothing was changed. Run `vario migrate` to apply.")
		return nil
	}

	// 2. Administrative credentials, kept in memory only.
	fmt.Fprintln(out)
	admin, _, err := promptAdmin(ctx, p, out, cc, o.admin, false)
	if errors.Is(err, errBack) || errors.Is(err, errCancelled) {
		fmt.Fprintln(out, "Cancelled; nothing was changed.")
		return nil
	}
	if err != nil {
		return err
	}
	actx, cancel := context.WithTimeout(ctx, checkTimeout(cc))
	plan, perr = admin.PlanSchema(actx)
	cancel()
	if perr != nil {
		fmt.Fprintf(out, "✗ %v\n  %s\n", perr, clickhouse.Hint(clickhouse.Classify(perr)))
		return withCode(ExitFailure, errors.New("migration not possible"))
	}
	fmt.Fprintf(out, "Current schema: %s\n", describeVersion(plan.Status))

	// Privileges of the operational user on Vario's objects.
	var grants *clickhouse.Plan
	var grantNote string
	if cc.Username != admin.User() {
		grants, grantNote = operationalGrantPlan(ctx, admin, cc)
	} else {
		grantNote = fmt.Sprintf("the collector uses the same account (%s) as this migration; a restricted operational account is recommended (`vario init`)", cc.Username)
	}

	if plan.Empty() && grants.Empty() {
		fmt.Fprintf(out, "✓ Vario schema in %s is up to date (version %d). Nothing to do.\n", cc.Database, clickhouse.SchemaVersion)
		if grantNote != "" {
			fmt.Fprintf(out, "! %s\n", grantNote)
		}
		return nil
	}
	if !plan.Empty() {
		fmt.Fprintf(out, "\nPlanned schema changes in %s:\n", cc.Database)
		printPlan(out, plan, o.dryRun)
	}
	if !grants.Empty() {
		fmt.Fprintf(out, "\nPrivileges missing for the collector user %s (it is not changed otherwise):\n", cc.Username)
		printPlan(out, grants, o.dryRun)
	}
	if grantNote != "" {
		fmt.Fprintf(out, "\n! %s\n", grantNote)
	}
	if o.dryRun {
		fmt.Fprintln(out, "\nDry run: nothing was changed. Run `vario migrate` to apply.")
		return nil
	}

	// 3. Explicit confirmation, separately for database creation, the
	// schema and changes to an existing user's privileges.
	if !p.interactive && !o.yes {
		return withCode(ExitUsage, errors.New("refusing to change ClickHouse without confirmation; review with --dry-run, then pass --yes"))
	}
	applySchema, applyGrants := !plan.Empty(), !grants.Empty()
	if p.interactive && !o.yes {
		if plan.Has(clickhouse.StepDatabase) {
			ok, err := p.confirm(fmt.Sprintf("\nDatabase %s does not exist. Create it (and Vario's schema in it)?", cc.Database), false)
			if err != nil {
				return err
			}
			if !ok {
				fmt.Fprintln(out, "Nothing was changed.")
				return nil
			}
		}
		if applySchema {
			if applySchema, err = confirmWithSQL(p, out, "Apply the schema changes above?", plan); err != nil {
				return err
			}
		}
		if applyGrants {
			if applyGrants, err = confirmWithSQL(p, out, fmt.Sprintf("Grant these privileges to %s?", cc.Username), grants); err != nil {
				return err
			}
		}
	}
	if !applySchema && !applyGrants {
		fmt.Fprintln(out, "Nothing was changed.")
		return nil
	}

	// 4. Apply, verify, and check the collector's own access.
	if applySchema {
		fmt.Fprintln(out, "\nApplying schema changes:")
		if res := applyPlan(ctx, out, admin, plan); !res.OK() {
			fmt.Fprintln(out, "\nFix the problem and run `vario migrate` again; completed steps are not repeated.")
			return withCode(ExitFailure, errors.New("migration failed"))
		}
		vctx, cancel := context.WithTimeout(ctx, checkTimeout(cc))
		st, err := admin.VerifyMigrated(vctx)
		cancel()
		if err != nil {
			fmt.Fprintf(out, "✗ verification failed: %v\n", err)
			return withCode(ExitFailure, errors.New("migration incomplete"))
		}
		fmt.Fprintf(out, "✓ schema verified: version %d, destination id %s\n", st.Recorded, st.DestinationID)
		for _, w := range st.OutdatedViews {
			fmt.Fprintf(out, "! view %s is still outdated\n", w)
		}
	}
	if applyGrants {
		fmt.Fprintln(out, "\nGranting privileges:")
		if res := applyPlan(ctx, out, admin, grants); !res.OK() {
			return withCode(ExitFailure, errors.New("granting privileges failed"))
		}
	}
	fmt.Fprintf(out, "\nChecking access as the collector user %s:\n", cc.Username)
	vctx, cancel := context.WithTimeout(ctx, checkTimeout(cc))
	rep, err := op.CheckConnection(vctx)
	cancel()
	if err != nil {
		fmt.Fprintf(out, "  ✗ %v\n    %s\n", err, clickhouse.Hint(clickhouse.Classify(err)))
		return withCode(ExitFailure, errors.New("the collector user cannot use the migrated schema"))
	}
	printReport(out, rep)
	if !rep.Ready() {
		return withCode(ExitFailure, errors.New("the collector user cannot deliver yet"))
	}
	fmt.Fprintln(out, "\nDone. A running collector picks up the schema at its next delivery attempt.")
	return nil
}

// operationalGrantPlan inspects the collector user's grants and returns the
// GRANT statements for missing privileges, or a note explaining why none
// can be applied.
func operationalGrantPlan(ctx context.Context, admin *clickhouse.Admin, cc config.ClickHouseConfig) (*clickhouse.Plan, string) {
	gctx, cancel := context.WithTimeout(ctx, checkTimeout(cc))
	defer cancel()
	u, ok, err := admin.LookupUser(gctx, cc.Username)
	switch {
	case err != nil:
		return nil, fmt.Sprintf("the privileges of %s could not be inspected (%v); the access check at the end verifies them", cc.Username, err)
	case !ok:
		return nil, fmt.Sprintf("user %s does not exist (or is not visible to %s); create it with `vario init`", cc.Username, admin.User())
	}
	as, err := admin.AssessGrants(gctx, cc.Username, cc.Database)
	if err != nil || len(as.Missing) == 0 {
		return nil, ""
	}
	if !u.Modifiable() {
		return nil, fmt.Sprintf("%s lacks privileges on new objects but is %s; grant them in the server configuration:\n%s",
			cc.Username, u.StorageNote(), requiredSQLGrants(cc.Database, cc.Username, as.Missing))
	}
	if as.Status == clickhouse.GrantsInconclusive {
		return clickhouse.GrantPlan(cc.Username, cc.Database, as.Missing), "the privileges of " + cc.Username + " could not be fully determined; the grants below are only needed if the access check fails"
	}
	return clickhouse.GrantPlan(cc.Username, cc.Database, as.Missing), ""
}

func requiredSQLGrants(db, user string, reqs []clickhouse.Requirement) string {
	var out string
	for _, s := range clickhouse.GrantPlan(user, db, reqs).Steps {
		out += "    " + s.SQL + ";\n"
	}
	return out
}

// confirmWithSQL asks to apply a plan, offering to show its SQL first.
func confirmWithSQL(p *prompter, out io.Writer, question string, plan *clickhouse.Plan) (bool, error) {
	for {
		c, err := p.choose("\n"+question, []string{"Apply", "Show the SQL statements", "Do not apply"}, 0)
		if err != nil {
			return false, err
		}
		switch c {
		case 0:
			return true, nil
		case 1:
			fmt.Fprintln(out)
			printPlan(out, plan, true)
		default:
			return false, nil
		}
	}
}

func describeVersion(st clickhouse.SchemaStatus) string {
	switch {
	case !st.DatabaseExists:
		return "the database does not exist (or is not visible)"
	case st.Complete():
		return fmt.Sprintf("version %d", st.Recorded)
	case st.Recorded > 0:
		return fmt.Sprintf("version %d, incomplete", st.Recorded)
	case st.Version() == 1:
		return "version 1 (created before schema versions were recorded)"
	case len(st.MissingTables) == len(clickhouse.OperationalTables()):
		return "no Vario schema"
	}
	return "incomplete (missing: " + fmt.Sprint(st.MissingTables) + ")"
}
