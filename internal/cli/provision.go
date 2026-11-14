package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/nedanwr/vario/internal/config"
	"github.com/nedanwr/vario/internal/storage/clickhouse"
)

// errBack returns to the previous step of an interactive flow.
var errBack = errors.New("back")

// adminOptions supply administrative credentials non-interactively.
type adminOptions struct {
	user          string
	passwordStdin bool
}

// promptAdmin asks for administrative credentials and authenticates. The
// password is read without echo (or from stdin with --admin-password-stdin)
// and kept only in the returned connection's memory; it is never written
// anywhere. Interactive callers may get errBack (change the endpoint) or
// errManual (continue without administrative access).
func promptAdmin(ctx context.Context, p *prompter, out io.Writer, cc config.ClickHouseConfig, o adminOptions, allowManual bool) (*clickhouse.Admin, string, error) {
	user := o.user
	if user == "" {
		user = "default"
	}
	if !p.interactive {
		if o.user == "" || !o.passwordStdin {
			return nil, "", withCode(ExitUsage, errors.New("administrative credentials are required: pass --admin-user and --admin-password-stdin"))
		}
		pw, err := readSecretStdin(p)
		if err != nil {
			return nil, "", err
		}
		admin, err := clickhouse.NewAdmin(cc, user, pw)
		if err != nil {
			return nil, "", err
		}
		ver, err := authenticate(ctx, admin, cc)
		if err != nil {
			return nil, "", fmt.Errorf("administrative login as %s failed (%s): %w", user, clickhouse.Classify(err), err)
		}
		return admin, ver, nil
	}
	fmt.Fprintln(out, "Administrative credentials are used only for this command and are never saved.")
	for {
		var err error
		if user, err = p.askValid("Administrative ClickHouse user", user, func(s string) error {
			if s == "" {
				return errors.New("a user name is required")
			}
			return nil
		}); err != nil {
			return nil, "", err
		}
		pw, err := p.secret(fmt.Sprintf("Password for %s (input hidden)", user))
		if err != nil {
			return nil, "", err
		}
		admin, err := clickhouse.NewAdmin(cc, user, pw)
		if err != nil {
			return nil, "", err
		}
		ver, err := authenticate(ctx, admin, cc)
		if err == nil {
			fmt.Fprintf(out, "  ✓ authenticated as %s (ClickHouse %s)\n\n", user, ver)
			return admin, ver, nil
		}
		kind := clickhouse.Classify(err)
		var ce *clickhouse.CheckError
		if errors.As(err, &ce) {
			kind = ce.Kind
		}
		fmt.Fprintf(out, "  ✗ %s failure: %v\n    %s\n", kind, err, adminHint(kind))
		opts := []string{"Retry", "Enter different administrative credentials", "Change the ClickHouse endpoint"}
		if allowManual {
			opts = append(opts, "Continue without administrative access (manually managed setup)")
		} else {
			opts = append(opts, "Cancel")
		}
		c, err := p.choose("\nWhat would you like to do?", opts, 0)
		if err != nil {
			return nil, "", err
		}
		fmt.Fprintln(out)
		switch c {
		case 0:
			// Retry with the same user; the password is asked again so that a
			// typo can be corrected.
		case 1:
			user = ""
		case 2:
			return nil, "", errBack
		case 3:
			if allowManual {
				return nil, "", errManual
			}
			return nil, "", errCancelled
		}
	}
}

var (
	errManual    = errors.New("manual setup")
	errCancelled = errors.New("cancelled; nothing was changed")
)

func adminHint(k clickhouse.Kind) string {
	if k == clickhouse.KindAuth {
		return "ClickHouse rejected the administrative credentials. Check the user name and password."
	}
	return clickhouse.Hint(k)
}

func authenticate(ctx context.Context, admin *clickhouse.Admin, cc config.ClickHouseConfig) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, checkTimeout(cc))
	defer cancel()
	return admin.Authenticate(cctx)
}

func checkTimeout(cc config.ClickHouseConfig) time.Duration {
	t := cc.Timeout.Duration
	if t <= 0 || t > 15*time.Second {
		t = 15 * time.Second
	}
	return t
}

func readSecretStdin(p *prompter) (string, error) {
	b, err := io.ReadAll(io.LimitReader(p.r, 4096))
	if err != nil {
		return "", err
	}
	return strings.TrimRight(string(b), "\r\n"), nil
}

var stepHeadings = map[clickhouse.StepKind]string{
	clickhouse.StepDatabase:   "Database",
	clickhouse.StepUser:       "User",
	clickhouse.StepPrivileges: "Privileges",
	clickhouse.StepSchema:     "Vario schema",
}

// printPlan lists the steps of a plan grouped by kind, optionally with their
// SQL. Secrets never appear: steps carry only a masked display form.
func printPlan(out io.Writer, p *clickhouse.Plan, showSQL bool) {
	last := clickhouse.StepKind("")
	for i, s := range p.Steps {
		if s.Kind != last {
			last = s.Kind
			fmt.Fprintf(out, "  %s\n", stepHeadings[s.Kind])
		}
		fmt.Fprintf(out, "    %2d. %s\n", i+1, s.Summary)
		if showSQL {
			for _, line := range strings.Split(s.SQL, "\n") {
				fmt.Fprintf(out, "          %s\n", line)
			}
		}
	}
}

// applyPlan applies a plan with progress output and reports a partial
// failure accurately. It returns the result; on failure the remaining steps
// (the failed one first) can be retried.
func applyPlan(ctx context.Context, out io.Writer, admin *clickhouse.Admin, p *clickhouse.Plan) clickhouse.ApplyResult {
	actx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	res := admin.Apply(actx, p, func(s clickhouse.Step, warning string, err error) {
		switch {
		case err != nil:
			fmt.Fprintf(out, "  ✗ %s: %v\n", s.Summary, err)
		case warning != "":
			fmt.Fprintf(out, "  ! %s: %s\n", s.Summary, warning)
		default:
			fmt.Fprintf(out, "  ✓ %s\n", s.Summary)
		}
	})
	if !res.OK() {
		kind := clickhouse.Classify(res.Err)
		fmt.Fprintf(out, "    %s\n", clickhouse.Hint(kind))
		if res.Partial() {
			fmt.Fprintf(out, "\n  %d of %d steps were applied before the failure and remain in effect (ClickHouse DDL is not transactional).\n",
				len(res.Applied), len(p.Steps))
		}
		if len(res.NotAttempted) > 0 {
			fmt.Fprintf(out, "  Not attempted: %d steps.\n", len(res.NotAttempted))
		}
	}
	return res
}

// remaining returns the steps of p that were not applied.
func remaining(p *clickhouse.Plan, res clickhouse.ApplyResult) *clickhouse.Plan {
	n := &clickhouse.Plan{Database: p.Database, Status: p.Status}
	if res.Failed != nil {
		n.Steps = append(n.Steps, *res.Failed)
	}
	n.Steps = append(n.Steps, res.NotAttempted...)
	return n
}

// printReport prints an operational connection report as check lines.
func printReport(out io.Writer, r *clickhouse.Report) {
	fmt.Fprintf(out, "  ✓ endpoint reachable\n  ✓ authenticated as %s (ClickHouse %s)\n", r.User, r.Version)
	st := r.Schema
	if st.DatabaseExists {
		fmt.Fprintf(out, "  ✓ database %s exists\n", r.Database)
	} else {
		fmt.Fprintf(out, "  ✗ database %s does not exist or %s has no access to it\n", r.Database, r.User)
	}
	if st.DatabaseExists {
		fmt.Fprintf(out, "  %s\n", schemaLine(st))
		for _, p := range st.Problems {
			fmt.Fprintf(out, "  ✗ incompatible schema: %s\n", p)
		}
	}
	if r.ReadOnly > 0 {
		fmt.Fprintf(out, "  ✗ sessions are read-only (readonly = %d)\n", r.ReadOnly)
	}
	if r.PrivilegesUnknown != "" {
		fmt.Fprintf(out, "  ! %s\n", r.PrivilegesUnknown)
	}
	var granted, missing []string
	for _, p := range r.Privileges {
		if p.Granted {
			granted = append(granted, p.Grant)
		} else {
			missing = append(missing, p.Grant)
		}
	}
	if len(granted) > 0 {
		fmt.Fprintf(out, "  ✓ privileges: %s\n", summarizeGrants(granted))
	}
	for _, g := range missing {
		fmt.Fprintf(out, "  ✗ missing privilege: %s\n", g)
	}
	switch {
	case r.Probe == "ok":
		fmt.Fprintf(out, "  ✓ inserts accepted by every Vario table with the collector's settings (probe without rows; nothing written)\n")
	case strings.HasPrefix(r.Probe, "not run"):
		fmt.Fprintf(out, "  · insert probe %s\n", r.Probe)
	case r.Probe != "":
		fmt.Fprintf(out, "  ✗ %s\n", r.Probe)
	}
	for _, u := range r.Unverified {
		fmt.Fprintf(out, "  ! not verified: %s\n", u)
	}
	for _, w := range r.Warnings {
		fmt.Fprintf(out, "  ! %s\n", w)
	}
	if len(r.Excess) > 0 {
		fmt.Fprintf(out, "  ! %s holds administrative privileges Vario does not need (%s); a restricted account is recommended (`vario init`, \"Create a dedicated Vario user\")\n",
			r.User, strings.Join(r.Excess, ", "))
	}
	switch {
	case r.Verified():
		fmt.Fprintf(out, "  ✓ ready: the collector's reads and inserts are accepted for %s\n", r.User)
	case r.Ready():
		fmt.Fprintf(out, "  ! no problems found, but not every check was conclusive (see above)\n")
	}
	if r.Probe == "ok" {
		fmt.Fprintf(out, "  · no rows are written by this check: quotas on written data, disk space and server limits are confirmed by the first delivery (`vario status`)\n")
	}
	kinds := map[clickhouse.Kind]bool{}
	for _, is := range r.Issues {
		if !kinds[is.Kind] {
			kinds[is.Kind] = true
			fmt.Fprintf(out, "    %s\n", clickhouse.Hint(is.Kind))
		}
	}
}

func schemaLine(st clickhouse.SchemaStatus) string {
	switch {
	case len(st.Problems) > 0:
		return "✗ Vario schema is incompatible"
	case st.Newer():
		return fmt.Sprintf("! Vario schema version %d is newer than this Vario (%d)", st.Recorded, clickhouse.SchemaVersion)
	case st.Complete():
		return fmt.Sprintf("✓ Vario schema version %d (up to date)", st.Recorded)
	case len(st.MissingTables) == len(clickhouse.OperationalTables()):
		return "✗ no Vario schema yet; run `vario migrate`"
	case len(st.MissingTables) > 0:
		return "✗ Vario schema incomplete (missing: " + strings.Join(st.MissingTables, ", ") + "); run `vario migrate`"
	}
	return fmt.Sprintf("· Vario schema version %d is usable; run `vario migrate` to update it to version %d", st.Version(), clickhouse.SchemaVersion)
}

// summarizeGrants shortens a list like "INSERT ON vario.sources, INSERT ON
// vario.projects, ..." to "INSERT on 8 tables in vario, ...".
func summarizeGrants(grants []string) string {
	type key struct{ access, db string }
	counts := map[key]int{}
	var order []key
	var other []string
	for _, g := range grants {
		access, obj, ok := strings.Cut(g, " ON ")
		db, table, ok2 := strings.Cut(obj, ".")
		if !ok || !ok2 || table == clickhouse.MetaTable || table == "*" {
			other = append(other, g)
			continue
		}
		k := key{access, db}
		if counts[k] == 0 {
			order = append(order, k)
		}
		counts[k]++
	}
	var parts []string
	for _, k := range order {
		if counts[k] == 1 {
			parts = append(parts, fmt.Sprintf("%s on 1 table in %s", k.access, k.db))
		} else {
			parts = append(parts, fmt.Sprintf("%s on %d tables in %s", k.access, counts[k], k.db))
		}
	}
	return strings.Join(append(parts, other...), ", ")
}

// printAssessment explains the result of inspecting a user's grants.
func printAssessment(out io.Writer, user, db string, as clickhouse.GrantAssessment) {
	switch as.Status {
	case clickhouse.GrantsSufficient:
		fmt.Fprintf(out, "  ✓ %s appears to have the privileges Vario needs on %s (granted directly or through default roles)\n", user, db)
	case clickhouse.GrantsInsufficient:
		fmt.Fprintf(out, "  ✗ %s is missing privileges Vario needs:\n", user)
	default:
		fmt.Fprintf(out, "  ? the privileges of %s could not be fully determined:\n", user)
	}
	if len(as.Missing) > 0 {
		var g []string
		for _, r := range as.Missing {
			g = append(g, r.String(db))
		}
		for _, line := range wrapList(g, 4) {
			fmt.Fprintf(out, "      %s\n", line)
		}
	}
	if len(as.Roles) > 0 {
		fmt.Fprintf(out, "    roles: %s\n", strings.Join(as.Roles, ", "))
	}
	for _, n := range as.Notes {
		fmt.Fprintf(out, "    · %s\n", n)
	}
	if len(as.Excess) > 0 {
		fmt.Fprintf(out, "    ! privileges beyond what Vario needs: %s\n", strings.Join(as.Excess, ", "))
	}
}

func wrapList(items []string, perLine int) []string {
	var out []string
	for i := 0; i < len(items); i += perLine {
		out = append(out, strings.Join(items[i:min(i+perLine, len(items))], ", "))
	}
	return out
}

// requiredSQL is what an administrator runs to provision the operational
// user by hand: the database, the user (with a placeholder password) and
// grants scoped to Vario's objects. The schema itself is created with
// `vario migrate`, whose --dry-run prints its statements.
func requiredSQL(db, user string, createUser bool, reqs []clickhouse.Requirement) string {
	var b strings.Builder
	fmt.Fprintf(&b, "CREATE DATABASE IF NOT EXISTS `%s`;\n", db)
	if createUser {
		fmt.Fprintf(&b, "CREATE USER `%s` IDENTIFIED WITH sha256_password BY '<password>' DEFAULT DATABASE `%s`;\n", user, db)
	}
	for _, s := range clickhouse.GrantPlan(user, db, reqs).Steps {
		b.WriteString(s.SQL + ";\n")
	}
	return b.String()
}
