package clickhouse

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/nedanwr/vario/internal/config"
	"github.com/nedanwr/vario/internal/model"
)

func testClient(t *testing.T, url string) *Client {
	t.Helper()
	cfg := config.Default().ClickHouse
	cfg.URL = url
	cfg.Username = "alice"
	cfg.Timeout = config.Duration{Duration: 5 * time.Second}
	c, err := New(cfg, "pw-123")
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func gz(s string) []byte {
	var b bytes.Buffer
	w := gzip.NewWriter(&b)
	w.Write([]byte(s))
	w.Close()
	return b.Bytes()
}

func TestInsertRequest(t *testing.T) {
	var got *http.Request
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r
		zr, err := gzip.NewReader(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		b, _ := io.ReadAll(zr)
		body = string(b)
	}))
	defer srv.Close()
	c := testClient(t, srv.URL)
	if err := c.Insert(context.Background(), "messages", gz(`{"a":1}`+"\n"), "tok-1"); err != nil {
		t.Fatal(err)
	}
	q := got.URL.Query()
	if q.Get("query") != "INSERT INTO `vario`.`messages` FORMAT JSONEachRow" || q.Get("insert_deduplication_token") != "tok-1" {
		t.Fatalf("query params: %v", q)
	}
	if got.Header.Get("X-ClickHouse-User") != "alice" || got.Header.Get("X-ClickHouse-Key") != "pw-123" || got.Header.Get("Content-Encoding") != "gzip" {
		t.Fatalf("headers: %v", got.Header)
	}
	if strings.Contains(got.URL.String(), "pw-123") {
		t.Fatal("password must never appear in the URL")
	}
	if body != `{"a":1}`+"\n" {
		t.Fatalf("body %q", body)
	}
}

func TestErrorClassificationAndSanitizing(t *testing.T) {
	status, code, msg := 0, "", ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if code != "" {
			w.Header().Set("X-ClickHouse-Exception-Code", code)
		}
		w.WriteHeader(status)
		io.WriteString(w, msg)
	}))
	defer srv.Close()
	c := testClient(t, srv.URL)

	status, code, msg = 400, "27", "Code: 27. DB::Exception: Cannot parse input: expected '\"' before: 'my secret transcript text': (at row 1)\nmore"
	err := c.Exec(context.Background(), "SELECT 1")
	if IsTransient(err) || strings.Contains(err.Error(), "secret transcript") || !strings.Contains(err.Error(), "row data omitted") {
		t.Fatalf("parse error: transient=%t %v", IsTransient(err), err)
	}
	status, code, msg = 503, "", "unavailable"
	if err := c.Exec(context.Background(), "SELECT 1"); !IsTransient(err) {
		t.Fatalf("503 should be transient: %v", err)
	}
	status, code, msg = 500, "241", "Code: 241. Memory limit exceeded"
	if err := c.Exec(context.Background(), "SELECT 1"); !IsTransient(err) {
		t.Fatalf("memory limit should be transient: %v", err)
	}
	status, code, msg = 403, "516", "Code: 516. Authentication failed"
	if err := c.Exec(context.Background(), "SELECT 1"); IsTransient(err) {
		t.Fatalf("auth failure should not be transient: %v", err)
	}
	srv.Close()
	if err := c.Exec(context.Background(), "SELECT 1"); !IsTransient(err) {
		t.Fatalf("connection failure should be transient: %v", err)
	}
}

var colRe = regexp.MustCompile(`(?m)^  ([a-z_]+) `)

// TestModelMatchesSchema ensures every JSON field of every row type is a column
// of its table, so inserts never silently drop data.
func TestModelMatchesSchema(t *testing.T) {
	ddl := tableDDL("vario")
	rows := map[string]any{
		model.TableSources: model.SourceRow{}, model.TableProjects: model.ProjectRow{}, model.TableConversations: model.ConversationRow{},
		model.TableRuns: model.RunRow{}, model.TableMessages: model.MessageRow{}, model.TableExecutionEvents: model.ExecutionEventRow{},
		model.TableProviderTurns: model.ProviderTurnRow{}, model.TableRawEvents: model.RawEventRow{},
	}
	for _, table := range model.Tables {
		cols := map[string]bool{}
		for _, m := range colRe.FindAllStringSubmatch(ddl[table], -1) {
			cols[m[1]] = true
		}
		for _, f := range jsonFields(reflect.TypeOf(rows[table])) {
			if !cols[f] {
				t.Errorf("%s: field %q has no column", table, f)
			}
		}
	}
}

func jsonFields(t reflect.Type) []string {
	var out []string
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.Anonymous {
			out = append(out, jsonFields(f.Type)...)
			continue
		}
		if tag := strings.Split(f.Tag.Get("json"), ",")[0]; tag != "" && tag != "-" {
			out = append(out, tag)
		}
	}
	return out
}
