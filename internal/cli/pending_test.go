package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/nedanwr/vario/internal/config"
	"github.com/nedanwr/vario/internal/sources/t3code/t3codetest"
)

// guidedUntilApplied answers a fresh guided setup that creates database
// vario and user vario with a generated password, up to and including
// applying the plan.
func guidedUntilApplied(url string) []string {
	return []string{
		url, "", "", "adminpw", // endpoint, guided, administrative user and password
		"2", "", // create database vario
		"", "", "", "", // create user vario, generate its password, do not show it
		"", // create the schema
		"", // apply
	}
}

func readPending(t *testing.T, cfgPath string) []pendingCredentials {
	t.Helper()
	b, err := os.ReadFile(pendingPath(cfgPath))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var s pendingStore
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && mode(t, pendingPath(cfgPath)) != 0o600 {
		t.Fatal("the pending credentials file must be readable only by the user")
	}
	if strings.Contains(string(b), "adminpw") {
		t.Fatal("administrative credentials must never be written")
	}
	return s.Users
}

// authenticates reports whether the fake accepts pw for user.
func authenticates(ch *fakeCH, user, pw string) bool {
	u := ch.user(user)
	if u == nil {
		return false
	}
	sum := sha256.Sum256([]byte(pw + u.salt))
	return strings.ToUpper(hex.EncodeToString(sum[:])) == u.hash
}

func TestInitInputEndsAfterUserCreatedThenResumes(t *testing.T) {
	cfgPath, _ := isolate(t)
	fix := t3codetest.New(t)
	ch, url := guidedFake(t)
	// Input ends at the summary, after the user was created.
	r := vario(t, answers(append(guidedUntilApplied(url), "", "", "")...), "init", "--t3code-path", fix.Path)
	if r.code == 0 || fileExists(cfgPath) {
		t.Fatalf("input ended before saving: %+v", r)
	}
	p := readPending(t, cfgPath)
	if len(p) != 1 || p[0].User != "vario" || p[0].Database != "vario" || !p[0].Generated || len(p[0].Password) != 32 {
		t.Fatalf("the new user's password must be kept: %+v", p)
	}
	if !authenticates(ch, "vario", p[0].Password) {
		t.Fatal("the kept password must be the one the user was created with")
	}
	noSecrets(t, r, p[0].Password, "adminpw")
	for _, want := range []string{"ClickHouse user vario was created at", "Its password is kept in", "Run `vario init` again to finish"} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("output missing %q:\n%s", want, r.stdout)
		}
	}

	// The next init offers to finish the setup with it; nothing is changed in ClickHouse.
	n := len(ch.changes())
	r = vario(t, answers(
		"",         // finish the setup with this user
		"", "", "", // mode, enable T3 Code, path
		"", // save
	), "init", "--t3code-path", fix.Path)
	if r.code != 0 {
		t.Fatalf("resumed init: %+v", r)
	}
	for _, want := range []string{"An earlier `vario init` created ClickHouse user vario", "Verifying the login as vario", "inserts accepted",
		"finishing an earlier guided setup", "✓ Configuration saved"} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("resumed output missing %q:\n%s", want, r.stdout)
		}
	}
	noSecrets(t, r, p[0].Password)
	if len(ch.changes()) != n {
		t.Fatalf("finishing the setup must not change ClickHouse: %v", ch.changes()[n:])
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if pw, _ := cfg.Password(); pw != p[0].Password || cfg.ClickHouse.Username != "vario" || cfg.ClickHouse.URL != url {
		t.Fatalf("the saved configuration must use the kept password: %+v", cfg.ClickHouse)
	}
	if fileExists(pendingPath(cfgPath)) {
		t.Fatal("the setup copy of the password must be removed once the configuration is saved")
	}
}

func TestInitQuitAfterUserCreatedKeepsPassword(t *testing.T) {
	cfgPath, _ := isolate(t)
	fix := t3codetest.New(t)
	_, url := guidedFake(t)
	r := vario(t, answers(append(guidedUntilApplied(url), "", "", "",
		"5", "1", // quit, then go back to the summary
		"5", "2", // quit for real
	)...), "init", "--t3code-path", fix.Path)
	if r.code != 0 || fileExists(cfgPath) {
		t.Fatalf("quit: %+v", r)
	}
	if !strings.Contains(r.stdout, "Go back to the summary") || !strings.Contains(r.stdout, "the password stays in") ||
		!strings.Contains(r.stdout, "Run `vario init` again to finish") {
		t.Fatalf("quitting must explain where the password is kept:\n%s", r.stdout)
	}
	if p := readPending(t, cfgPath); len(p) != 1 {
		t.Fatalf("the password must be kept after quitting: %+v", p)
	}

	// Going back and saving instead removes the setup copy.
	cfgPath, _ = isolate(t)
	_, url = guidedFake(t)
	r = vario(t, answers(append(guidedUntilApplied(url), "", "", "", "5", "1", "1")...), "init", "--t3code-path", fix.Path)
	if r.code != 0 || !fileExists(cfgPath) || fileExists(pendingPath(cfgPath)) {
		t.Fatalf("save after going back: %+v", r)
	}
}

func TestInitCancelBeforeApplyKeepsNothing(t *testing.T) {
	cfgPath, _ := isolate(t)
	ch, url := guidedFake(t)
	a := guidedUntilApplied(url)
	a[len(a)-1] = "4" // cancel instead of applying
	r := vario(t, answers(a...), "init")
	if r.code != 0 || !strings.Contains(r.stdout, "Setup cancelled.\nNothing was saved.") {
		t.Fatalf("cancel: %+v", r)
	}
	if len(ch.changes()) != 0 || ch.user("vario") != nil || fileExists(pendingPath(cfgPath)) || fileExists(cfgPath) {
		t.Fatal("cancelling before applying must change nothing and keep no password")
	}
}

func TestInitUserCreationFailureKeepsNoPassword(t *testing.T) {
	cfgPath, _ := isolate(t)
	ch, url := guidedFake(t)
	ch.failOn("CREATE USER")
	r := vario(t, answers(append(guidedUntilApplied(url), "3")...), "init") // quit after the failure
	if r.code != 0 || ch.user("vario") != nil {
		t.Fatalf("failed user creation: %+v", r)
	}
	if fileExists(pendingPath(cfgPath)) {
		t.Fatal("a password must not be kept for a user that was certainly not created")
	}
	if !strings.Contains(r.stdout, "Changes confirmed earlier were applied in ClickHouse") || strings.Contains(r.stdout, "Its password is kept") {
		t.Fatalf("output:\n%s", r.stdout)
	}
}

func TestInitGrantFailureKeepsPassword(t *testing.T) {
	cfgPath, _ := isolate(t)
	ch, url := guidedFake(t)
	ch.failOn("GRANT")
	r := vario(t, answers(append(guidedUntilApplied(url), "3")...), "init") // quit after the failure
	if r.code != 0 || ch.user("vario") == nil {
		t.Fatalf("failed grants: %+v", r)
	}
	p := readPending(t, cfgPath)
	if len(p) != 1 || !authenticates(ch, "vario", p[0].Password) {
		t.Fatalf("the created user's password must be kept: %+v", p)
	}
	if !strings.Contains(r.stdout, "ClickHouse user vario was created at") {
		t.Fatalf("output:\n%s", r.stdout)
	}
	noSecrets(t, r, p[0].Password)
}

func TestInitDoesNotCreateUserWhosePasswordCannotBeKept(t *testing.T) {
	cfgPath, _ := isolate(t)
	ch, url := guidedFake(t)
	// The pending credentials file cannot be read or written.
	if err := os.MkdirAll(pendingPath(cfgPath), 0o700); err != nil {
		t.Fatal(err)
	}
	r := vario(t, answers(guidedUntilApplied(url)...), "init")
	if r.code == 0 || !strings.Contains(r.stdout, "cannot be kept safely") {
		t.Fatalf("init: %+v", r)
	}
	if ch.user("vario") != nil || len(ch.changes()) != 0 {
		t.Fatalf("nothing may be applied when the password cannot be kept: %v", ch.changes())
	}
}

// A crash or SIGKILL after the password was kept but before ClickHouse
// created the user: the next init creates it with the kept password.
func TestInitResumesWhenUserWasNotCreated(t *testing.T) {
	cfgPath, _ := isolate(t)
	fix := t3codetest.New(t)
	ch, url := guidedFake(t)
	st := &pendingStore{path: pendingPath(cfgPath)}
	if err := st.put(pendingCredentials{Endpoint: url, Database: "vario", User: "vario", Password: "kept-Password-123", Generated: true, Created: time.Now()}); err != nil {
		t.Fatal(err)
	}
	r := vario(t, answers(
		"",                    // finish the setup with this user
		"",                    // the login fails: continue with the connection setup
		"", "", "", "adminpw", // keep the endpoint, guided, administrative credentials
		"2", "", // create database vario
		"", "", // create user vario
		"",                 // use the kept password
		"", "", "", "", "", // schema, apply, mode, enable T3 Code, path
		"", // save
	), "init", "--t3code-path", fix.Path)
	if r.code != 0 {
		t.Fatalf("resume: %+v", r)
	}
	if !strings.Contains(r.stdout, "guided setup can\n  create it now with the saved password") || !strings.Contains(r.stdout, "Use the password saved when an earlier setup created vario") {
		t.Fatalf("output:\n%s", r.stdout)
	}
	if !authenticates(ch, "vario", "kept-Password-123") {
		t.Fatal("the user must be created with the kept password")
	}
	cfg, _ := config.Load(cfgPath)
	if pw, _ := cfg.Password(); pw != "kept-Password-123" || fileExists(pendingPath(cfgPath)) {
		t.Fatal("the kept password must be saved and its setup copy removed")
	}
	noSecrets(t, r, "kept-Password-123")
}

func TestInitPendingPasswordHousekeeping(t *testing.T) {
	_, cfgPath := initFixture(t)
	cfg, _ := config.Load(cfgPath)
	if err := cfg.WritePassword("pw1"); err != nil {
		t.Fatal(err)
	}
	st := &pendingStore{path: pendingPath(cfgPath)}
	other := pendingCredentials{Endpoint: "https://other.example.com", Database: "x", User: "other", Password: "pw2", Created: time.Now()}
	for _, p := range []pendingCredentials{{Endpoint: unreachable, Database: "vario", User: "default", Password: "pw1", Created: time.Now()}, other} {
		if err := st.put(p); err != nil {
			t.Fatal(err)
		}
	}
	// Non-interactive init never touches unresolved entries but mentions them.
	r := vario(t, "", "init", "--non-interactive", "--update", "--skip-connection-check")
	if r.code != 0 || !strings.Contains(r.stderr, "An earlier `vario init` created ClickHouse user other") {
		t.Fatalf("non-interactive: %+v", r)
	}
	if p := readPending(t, cfgPath); len(p) != 1 || p[0].User != "other" {
		t.Fatalf("an entry the saved configuration already uses is removed, others are kept: %+v", p)
	}
	noSecrets(t, r, "pw1", "pw2")
	// Deleting the remaining entry requires confirmation.
	r = vario(t, answers("1", "3", "n"), "init")
	if p := readPending(t, cfgPath); len(p) != 1 || !strings.Contains(r.stdout, "DROP USER other") {
		t.Fatalf("declined deletion must keep the password: %+v\n%s", p, r.stdout)
	}
	vario(t, answers("1", "3", "y"), "init")
	if fileExists(pendingPath(cfgPath)) {
		t.Fatal("confirmed deletion must remove the file")
	}
}

func TestInitUnreadablePendingFileIsKept(t *testing.T) {
	cfgPath, _ := isolate(t)
	if err := config.WritePrivateFile(pendingPath(cfgPath), []byte("{not json")); err != nil {
		t.Fatal(err)
	}
	ch, url := guidedFake(t)
	r := vario(t, answers(guidedUntilApplied(url)...), "init")
	if !strings.Contains(r.stdout, "was left untouched") || ch.user("vario") != nil {
		t.Fatalf("an unreadable pending file must block creating users and stay untouched: %+v", r)
	}
	if readFile(t, pendingPath(cfgPath)) != "{not json" {
		t.Fatal("the unreadable file must not be overwritten")
	}
}
