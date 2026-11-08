// Package clickhouse delivers records to ClickHouse over its HTTP(S) interface.
//
// The HTTP interface is used instead of the native protocol because it needs no
// driver dependency, works through standard TLS terminators and proxies, and
// is available on every self-hosted and managed ClickHouse deployment.
package clickhouse

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/nedanwr/vario/internal/buildinfo"
	"github.com/nedanwr/vario/internal/config"
)

type Client struct {
	base     *url.URL
	database string
	user     string
	password string
	http     *http.Client
}

// New creates a client. The password is sent only in request headers.
func New(cfg config.ClickHouseConfig, password string) (*Client, error) {
	u, err := url.Parse(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("clickhouse url: %w", err)
	}
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: cfg.TLSInsecureSkipVerify} //nolint:gosec // explicit user opt-in
	if cfg.CAFile != "" {
		pem, err := os.ReadFile(cfg.CAFile)
		if err != nil {
			return nil, fmt.Errorf("read ca_file: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, errors.New("ca_file contains no PEM certificates")
		}
		tlsCfg.RootCAs = pool
	}
	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSClientConfig:       tlsCfg,
		TLSHandshakeTimeout:   10 * time.Second,
		MaxIdleConns:          2,
		IdleConnTimeout:       60 * time.Second,
		ResponseHeaderTimeout: cfg.Timeout.Duration,
		ForceAttemptHTTP2:     true,
	}
	return &Client{
		base:     u,
		database: cfg.Database,
		user:     cfg.Username,
		password: password,
		http: &http.Client{
			Transport: transport,
			Timeout:   cfg.Timeout.Duration,
			// ClickHouse never redirects. Following one would forward the
			// X-ClickHouse-Key header to the redirect target (Go only strips
			// standard auth headers) and could downgrade https to http.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}, nil
}

func (c *Client) Database() string { return c.database }

// Error is a ClickHouse or transport failure with a sanitized message.
type Error struct {
	Status    int
	Code      int
	Message   string
	Transient bool
}

func (e *Error) Error() string {
	switch {
	case e.Code != 0:
		return fmt.Sprintf("clickhouse error %d (HTTP %d): %s", e.Code, e.Status, e.Message)
	case e.Status != 0:
		return fmt.Sprintf("clickhouse HTTP %d: %s", e.Status, e.Message)
	default:
		return "clickhouse: " + e.Message
	}
}

// IsTransient reports whether retrying the same request may succeed without user action.
func IsTransient(err error) bool {
	var e *Error
	if errors.As(err, &e) {
		return e.Transient
	}
	return true
}

var (
	codeRe     = regexp.MustCompile(`^Code: (\d+)\. `)
	dataLeakRe = regexp.MustCompile(`(?i)(: \(at row|before: |row \d+:|parsed text:).*`)
)

// sanitize keeps ClickHouse error text short and strips echoed row data so that
// conversation content never reaches logs or the spool's error column.
func sanitize(msg string) string {
	msg = strings.TrimSpace(msg)
	if i := strings.IndexByte(msg, '\n'); i >= 0 {
		msg = msg[:i]
	}
	msg = dataLeakRe.ReplaceAllString(msg, " [row data omitted]")
	if len(msg) > 300 {
		msg = msg[:300] + "..."
	}
	return msg
}

// Transient ClickHouse error codes: timeouts, overload, too many parts, memory limits, readonly replicas.
var transientCodes = map[int]bool{159: true, 164: true, 202: true, 203: true, 209: true, 210: true, 241: true, 242: true, 252: true, 285: true, 319: true, 425: true}

func (c *Client) do(ctx context.Context, method string, params url.Values, body io.Reader, gzipped bool) ([]byte, error) {
	u := *c.base
	u.Path = strings.TrimSuffix(u.Path, "/") + "/"
	u.RawQuery = params.Encode()
	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-ClickHouse-User", c.user)
	if c.password != "" {
		req.Header.Set("X-ClickHouse-Key", c.password)
	}
	req.Header.Set("User-Agent", buildinfo.UserAgent())
	if gzipped {
		req.Header.Set("Content-Encoding", "gzip")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, &Error{Message: sanitizeTransport(err), Transient: true}
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, &Error{Status: resp.StatusCode, Message: "read response: " + err.Error(), Transient: true}
	}
	if resp.StatusCode == http.StatusOK {
		return data, nil
	}
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		return nil, &Error{Status: resp.StatusCode, Message: "unexpected redirect (not followed; check clickhouse.url)"}
	}
	e := &Error{Status: resp.StatusCode, Message: sanitize(string(data))}
	if code := resp.Header.Get("X-ClickHouse-Exception-Code"); code != "" {
		e.Code, _ = strconv.Atoi(code)
	} else if m := codeRe.FindStringSubmatch(string(data)); m != nil {
		e.Code, _ = strconv.Atoi(m[1])
	}
	e.Transient = resp.StatusCode >= 500 && e.Code == 0 || resp.StatusCode == http.StatusTooManyRequests ||
		resp.StatusCode == http.StatusServiceUnavailable || transientCodes[e.Code]
	return nil, e
}

func sanitizeTransport(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Op + ": " + ue.Err.Error()
	}
	return err.Error()
}

// Ping checks that the HTTP endpoint is reachable (no authentication involved).
func (c *Client) Ping(ctx context.Context) error {
	u := *c.base
	u.Path = strings.TrimSuffix(u.Path, "/") + "/ping"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return &Error{Message: sanitizeTransport(err), Transient: true}
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1024))
	if resp.StatusCode != http.StatusOK {
		return &Error{Status: resp.StatusCode, Message: "unexpected ping response", Transient: true}
	}
	return nil
}

// Exec runs a statement that returns no rows.
func (c *Client) Exec(ctx context.Context, query string) error {
	_, err := c.do(ctx, http.MethodPost, url.Values{}, strings.NewReader(query), false)
	return err
}

// Query runs a SELECT and decodes rows from ClickHouse's JSON output format.
func (c *Client) Query(ctx context.Context, query string, rows any) error {
	params := url.Values{}
	params.Set("default_format", "JSON")
	params.Set("output_format_json_quote_64bit_integers", "0")
	data, err := c.do(ctx, http.MethodPost, params, strings.NewReader(query), false)
	if err != nil {
		return err
	}
	var env struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(data, &env); err != nil {
		return fmt.Errorf("decode clickhouse response: %w", err)
	}
	return json.Unmarshal(env.Data, rows)
}

// ServerVersion returns the ClickHouse server version and verifies authentication.
func (c *Client) ServerVersion(ctx context.Context) (string, error) {
	var rows []struct {
		V string `json:"v"`
	}
	if err := c.Query(ctx, "SELECT version() AS v", &rows); err != nil {
		return "", err
	}
	if len(rows) == 0 {
		return "", errors.New("empty version response")
	}
	return rows[0].V, nil
}

// Insert sends a gzip-compressed JSONEachRow body. The deduplication token makes
// a retried insert of the same batch idempotent at the block level; row-level
// correctness additionally relies on ReplacingMergeTree versions.
func (c *Client) Insert(ctx context.Context, table string, gzBody []byte, token string) error {
	params := url.Values{}
	params.Set("query", fmt.Sprintf("INSERT INTO %s.%s FORMAT JSONEachRow", quoteIdent(c.database), quoteIdent(table)))
	params.Set("input_format_skip_unknown_fields", "1")
	params.Set("input_format_null_as_default", "1")
	params.Set("date_time_input_format", "best_effort")
	params.Set("async_insert", "0")
	if token != "" {
		params.Set("insert_deduplication_token", token)
	}
	_, err := c.do(ctx, http.MethodPost, params, bytes.NewReader(gzBody), true)
	return err
}

func quoteIdent(s string) string { return "`" + strings.ReplaceAll(s, "`", "``") + "`" }
