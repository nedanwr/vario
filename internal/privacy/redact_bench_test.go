package privacy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// benchText resembles tool output: source code and logs with a few secrets.
var benchText = strings.Repeat(`func handler(w http.ResponseWriter, r *http.Request) {
	token := r.Header.Get("Authorization") // e.g. Bearer abcdefghijklmnopqrstuvwxyz012345
	if err := db.QueryRow("SELECT id FROM users WHERE email = $1", email).Scan(&id); err != nil {
		log.Printf("lookup failed: %v", err)
	}
}
export DB_PASSWORD=s3cr3t-value-123
2026-03-01T10:00:00Z INFO request served path=/api/v1/items status=200 duration=12ms
`, 200)

func BenchmarkRedact(b *testing.B) {
	p := newPolicy(b, nil)
	b.SetBytes(int64(len(benchText)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		p.Redact(benchText)
	}
}

// BenchmarkRedactSourceCode redacts this repository's Go sources, which is
// representative of tool output in full mode.
func BenchmarkRedactSourceCode(b *testing.B) {
	files, _ := filepath.Glob("../*/*.go")
	var sb strings.Builder
	for _, f := range files {
		data, _ := os.ReadFile(f)
		sb.Write(data)
	}
	text := sb.String()
	p := newPolicy(b, nil)
	b.SetBytes(int64(len(text)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		p.Redact(text)
	}
}
