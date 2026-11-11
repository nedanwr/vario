package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
)

// fakeCH is a ClickHouse HTTP endpoint for CLI tests. It keeps enough state
// to run the guided setup end to end: databases, users (with password or
// salted SHA-256 authentication, access storage and an admin flag), grants
// on db.table / db.*, objects created by DDL (with their columns, engine and
// comment, so Vario's schema inspection sees what was created), vario_meta
// rows and the readonly setting. Users that are not admins may only run
// SELECT, CHECK GRANT and inserts they hold privileges for, and see only
// objects they have privileges on.
type fakeCH struct {
	mu        sync.Mutex
	password  string // password of any user not listed in users (an admin)
	users     map[string]*fakeUser
	databases map[string]bool
	objects   map[string]map[string]*fakeObj // db -> name -> object
	meta      map[string][]string            // db -> "key|value"
	queries   []string                       // "user: query"
	stmts     []string                       // statements that changed state
	failing   []string                       // prefixes of statements that fail
}

type fakeUser struct {
	password, hash, salt string
	storage              string
	admin                bool
	readonly             int
	grants               map[string]bool // "ACCESS db.table" or "ACCESS db.*"
}

type fakeObj struct {
	engine, comment        string
	sortingKey, engineFull string
	cols                   [][2]string
}

func newFakeCH(t *testing.T, password string, dbs ...string) (*fakeCH, string) {
	f := &fakeCH{password: password, users: map[string]*fakeUser{}, databases: map[string]bool{"system": true, "default": true, "INFORMATION_SCHEMA": true, "information_schema": true},
		objects: map[string]map[string]*fakeObj{}, meta: map[string][]string{}}
	for _, d := range dbs {
		f.databases[d] = true
	}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, srv.URL
}

func (f *fakeCH) addUser(name, password, storage string, admin bool) *fakeUser {
	f.mu.Lock()
	defer f.mu.Unlock()
	u := &fakeUser{password: password, storage: storage, admin: admin, grants: map[string]bool{}}
	f.users[name] = u
	return u
}

func (f *fakeCH) grant(user string, grants ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, g := range grants {
		f.users[user].grants[g] = true
	}
}

// changes returns the statements that changed state.
func (f *fakeCH) changes() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.stmts...)
}

func (f *fakeCH) failOn(prefixes ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failing = prefixes
}

func (f *fakeCH) user(name string) *fakeUser {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.users[name]
}

var (
	reDB        = regexp.MustCompile("^CREATE DATABASE IF NOT EXISTS `([^`]+)`$")
	reTable     = regexp.MustCompile("(?s)^CREATE TABLE IF NOT EXISTS `([^`]+)`\\.`([^`]+)` \\((.*)\\) ENGINE = (\\w+)")
	reView      = regexp.MustCompile("(?s)^CREATE (?:OR REPLACE )?VIEW (?:IF NOT EXISTS )?`([^`]+)`\\.`([^`]+)` AS .* COMMENT '([^']*)'$")
	reComment   = regexp.MustCompile("COMMENT '([^']*)'$")
	reEngine    = regexp.MustCompile(`(?m)^\) ENGINE = (.+)$`)
	reOrderBy   = regexp.MustCompile(`(?m)^ORDER BY (.+)$`)
	reCol       = regexp.MustCompile(`^  ([a-z_]+) (.+?),?$`)
	reMeta      = regexp.MustCompile("^INSERT INTO `([^`]+)`\\.`vario_meta` \\(key, value\\) VALUES (.*)$")
	rePair      = regexp.MustCompile(`\('([^']*)', '([^']*)'\)`)
	reUser      = regexp.MustCompile("^CREATE USER `([^`]+)` IDENTIFIED WITH sha256_hash BY '([0-9A-F]+)' SALT '([0-9A-F]+)'")
	reGrant     = regexp.MustCompile("^GRANT (.+) ON `([^`]+)`\\.`([^`]+)` TO `([^`]+)`$")
	reCheck     = regexp.MustCompile("^CHECK GRANT (.+) ON (?:`([^`]+)`|\\*)\\.(?:`([^`]+)`|\\*)$")
	reWhereDB   = regexp.MustCompile(`database = '([^']*)'`)
	reWhereName = regexp.MustCompile(`name = '([^']*)'`)
	reMetaRead  = regexp.MustCompile("FROM `([^`]+)`\\.`vario_meta`")
	reProbe     = regexp.MustCompile("^INSERT INTO `([^`]+)`\\.`([^`]+)` FORMAT JSONEachRow$")
	reGrantUser = regexp.MustCompile(`user_name = '([^']*)'`)
)

func (f *fakeCH) authenticate(r *http.Request) (string, *fakeUser, bool) {
	name, key := r.Header.Get("X-ClickHouse-User"), r.Header.Get("X-ClickHouse-Key")
	if u, ok := f.users[name]; ok {
		if u.hash != "" {
			sum := sha256.Sum256([]byte(key + u.salt))
			return name, u, strings.ToUpper(hex.EncodeToString(sum[:])) == u.hash
		}
		return name, u, key == u.password
	}
	return name, &fakeUser{admin: true, grants: map[string]bool{}}, key == f.password
}

func (u *fakeUser) has(access, db, table string) bool {
	return u.admin || u.grants[access+" "+db+"."+table] || u.grants[access+" "+db+".*"]
}

func (u *fakeUser) sees(db, table string) bool {
	if u.admin {
		return true
	}
	for g := range u.grants {
		_, obj, _ := strings.Cut(g, " ")
		if obj == db+"."+table || obj == db+".*" {
			return true
		}
	}
	return false
}

func (f *fakeCH) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/ping" {
		io.WriteString(w, "Ok.\n")
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	fail := func(status, code int, msg string) {
		w.Header().Set("X-ClickHouse-Exception-Code", fmt.Sprint(code))
		w.WriteHeader(status)
		fmt.Fprintf(w, "Code: %d. DB::Exception: %s", code, msg)
	}
	name, u, ok := f.authenticate(r)
	if !ok {
		fail(http.StatusForbidden, 516, name+": Authentication failed: password is incorrect, or there is no user with such name. (AUTHENTICATION_FAILED)")
		return
	}
	b, _ := io.ReadAll(r.Body)
	q := string(b)
	if qp := r.URL.Query().Get("query"); qp != "" {
		q = qp
	}
	f.queries = append(f.queries, name+": "+q)
	var data []map[string]any
	reply := func() { _ = json.NewEncoder(w).Encode(map[string]any{"data": data}) }
	switch {
	case strings.Contains(q, "version()"):
		data = append(data, map[string]any{"v": "25.8.0.1"})
	case strings.Contains(q, "getSetting('readonly')"):
		data = append(data, map[string]any{"r": fmt.Sprint(u.readonly)})
	case strings.Contains(q, "FROM system.databases WHERE"):
		n := 0
		if m := reWhereName.FindStringSubmatch(q); m != nil && f.databases[m[1]] && (u.admin || u.sees(m[1], "*") || f.anyGrantIn(u, m[1])) {
			n = 1
		}
		data = append(data, map[string]any{"n": n})
	case strings.Contains(q, "FROM system.databases"):
		var names []string
		for d := range f.databases {
			names = append(names, d)
		}
		sort.Strings(names)
		for _, d := range names {
			data = append(data, map[string]any{"name": d})
		}
	case strings.Contains(q, "FROM system.tables"):
		db := reWhereDB.FindStringSubmatch(q)[1]
		for n, o := range f.objects[db] {
			if u.sees(db, n) {
				data = append(data, map[string]any{"name": n, "engine": o.engine, "as_select": "", "comment": o.comment,
					"sorting_key": o.sortingKey, "engine_full": o.engineFull})
			}
		}
	case strings.Contains(q, "FROM system.columns"):
		db := reWhereDB.FindStringSubmatch(q)[1]
		for n, o := range f.objects[db] {
			if u.sees(db, n) {
				for _, c := range o.cols {
					data = append(data, map[string]any{"table": n, "name": c[0], "type": c[1]})
				}
			}
		}
	case reMetaRead.MatchString(q) && strings.HasPrefix(q, "SELECT"):
		db := reMetaRead.FindStringSubmatch(q)[1]
		if !u.has("SELECT", db, "vario_meta") {
			fail(500, 497, "Not enough privileges")
			return
		}
		for _, m := range f.meta[db] {
			k, v, _ := strings.Cut(m, "|")
			data = append(data, map[string]any{"key": k, "value": v})
		}
	case strings.Contains(q, "FROM system.users"):
		if !u.admin {
			fail(500, 497, "Not enough privileges. To execute this query, it's necessary to have the grant SHOW USERS ON *.*")
			return
		}
		want := ""
		if m := reWhereName.FindStringSubmatch(q); m != nil {
			want = m[1]
		}
		var names []string
		for n := range f.users {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			if want == "" || want == n {
				data = append(data, map[string]any{"name": n, "storage": f.users[n].storage})
			}
		}
	case strings.Contains(q, "FROM system.role_grants"):
	case strings.Contains(q, "FROM system.grants"):
		m := reGrantUser.FindStringSubmatch(q)
		if gu, ok := f.users[m[1]]; ok {
			var gs []string
			for g := range gu.grants {
				gs = append(gs, g)
			}
			sort.Strings(gs)
			for _, g := range gs {
				access, obj, _ := strings.Cut(g, " ")
				db, table, _ := strings.Cut(obj, ".")
				row := map[string]any{"user_name": m[1], "role_name": nil, "access_type": access, "database": db, "table": table, "column": nil, "is_partial_revoke": 0, "grant_option": 0}
				if table == "*" {
					row["table"] = nil
				}
				data = append(data, row)
			}
			if gu.admin {
				data = append(data, map[string]any{"user_name": m[1], "role_name": nil, "access_type": "ALL", "database": nil, "table": nil, "column": nil, "is_partial_revoke": 0, "grant_option": 1})
			}
		}
	case strings.HasPrefix(q, "CHECK GRANT"):
		m := reCheck.FindStringSubmatch(q)
		res := 0
		switch {
		case m == nil:
		case m[2] == "" && u.admin:
			res = 1
		case m[2] != "":
			table := m[3]
			if table == "" {
				table = "*"
			}
			if u.has(m[1], m[2], table) {
				res = 1
			}
		}
		data = append(data, map[string]any{"result": res})
	case reProbe.MatchString(q) && len(b) == 0:
		m := reProbe.FindStringSubmatch(q)
		switch {
		case u.readonly > 0:
			fail(500, 164, "Cannot execute query in readonly mode")
		case f.objects[m[1]][m[2]] == nil:
			fail(404, 60, "Unknown table")
		case !u.has("INSERT", m[1], m[2]):
			fail(500, 497, "Not enough privileges")
		}
		return
	case strings.HasPrefix(q, "SELECT"):
	default:
		if !u.admin {
			fail(500, 497, name+": Not enough privileges. (ACCESS_DENIED)")
			return
		}
		for _, p := range f.failing {
			if strings.HasPrefix(q, p) {
				fail(500, 999, "simulated failure")
				return
			}
		}
		if err := f.apply(q); err != nil {
			fail(500, 493, err.Error())
			return
		}
		f.stmts = append(f.stmts, q)
		return
	}
	reply()
}

func (f *fakeCH) anyGrantIn(u *fakeUser, db string) bool {
	for g := range u.grants {
		if strings.Contains(g, " "+db+".") {
			return true
		}
	}
	return false
}

// apply executes a state-changing statement issued by an admin.
func (f *fakeCH) apply(q string) error {
	switch {
	case reDB.MatchString(q):
		f.databases[reDB.FindStringSubmatch(q)[1]] = true
	case reTable.MatchString(q):
		m := reTable.FindStringSubmatch(q)
		if f.objects[m[1]] == nil {
			f.objects[m[1]] = map[string]*fakeObj{}
		}
		if _, ok := f.objects[m[1]][m[2]]; ok {
			return nil
		}
		o := &fakeObj{engine: m[4]}
		if strings.HasPrefix(m[4], "Replacing") {
			o.engine = "ReplacingMergeTree"
		}
		if c := reComment.FindStringSubmatch(q); c != nil {
			o.comment = c[1]
		}
		// As ClickHouse reports them: "a, b" and "Engine(args) ORDER BY (a, b)".
		if e, ob := reEngine.FindStringSubmatch(q), reOrderBy.FindStringSubmatch(q); e != nil && ob != nil {
			o.sortingKey = strings.Trim(ob[1], "()")
			o.engineFull = e[1] + " ORDER BY " + ob[1]
		}
		for _, line := range strings.Split(q, "\n") {
			if cm := reCol.FindStringSubmatch(line); cm != nil {
				typ := cm[2]
				for _, cut := range []string{" COMMENT ", " CODEC(", " DEFAULT "} {
					if i := strings.Index(typ, cut); i >= 0 {
						typ = typ[:i]
					}
				}
				o.cols = append(o.cols, [2]string{cm[1], strings.TrimSpace(typ)})
			}
		}
		f.objects[m[1]][m[2]] = o
	case reView.MatchString(q):
		m := reView.FindStringSubmatch(q)
		if f.objects[m[1]] == nil {
			f.objects[m[1]] = map[string]*fakeObj{}
		}
		f.objects[m[1]][m[2]] = &fakeObj{engine: "View", comment: m[3]}
	case reMeta.MatchString(q):
		m := reMeta.FindStringSubmatch(q)
		for _, p := range rePair.FindAllStringSubmatch(m[2], -1) {
			f.meta[m[1]] = append(f.meta[m[1]], p[1]+"|"+p[2])
		}
	case reUser.MatchString(q):
		m := reUser.FindStringSubmatch(q)
		if _, ok := f.users[m[1]]; ok {
			return fmt.Errorf("user `%s`: cannot insert because user `%s` already exists in local_directory. (USER_ALREADY_EXISTS)", m[1], m[1])
		}
		f.users[m[1]] = &fakeUser{hash: m[2], salt: m[3], storage: "local_directory", grants: map[string]bool{}}
	case reGrant.MatchString(q):
		m := reGrant.FindStringSubmatch(q)
		gu, ok := f.users[m[4]]
		if !ok {
			return fmt.Errorf("there is no user `%s`", m[4])
		}
		if gu.storage != "local_directory" {
			return fmt.Errorf("cannot update user `%s` in users_xml because this storage is readonly", m[4])
		}
		for _, a := range strings.Split(m[1], ", ") {
			gu.grants[a+" "+m[2]+"."+m[3]] = true
		}
	}
	return nil
}
