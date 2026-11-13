package buildinfo

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestInfoString(t *testing.T) {
	for _, tc := range []struct {
		name string
		info Info
		want string
	}{
		{
			name: "release build",
			info: Info{Version: "v0.1.0-beta.1", Commit: "57fab9a0123456789abcdef0123456789abcdef0", Date: "2026-11-13T12:00:00Z", GoVersion: "go1.25.5", Platform: "darwin/arm64"},
			want: "vario v0.1.0-beta.1 (commit 57fab9a01234, built 2026-11-13T12:00:00Z, go1.25.5, darwin/arm64)",
		},
		{
			name: "modified tree",
			info: Info{Version: "v0.1.0-beta.1", Commit: "57fab9a0123456789abcdef0", Date: "2026-11-13T12:00:00Z", GoVersion: "go1.25.5", Platform: "linux/amd64", Modified: true},
			want: "vario v0.1.0-beta.1 (commit 57fab9a01234-dirty, built 2026-11-13T12:00:00Z, go1.25.5, linux/amd64)",
		},
		{
			name: "no metadata",
			info: Info{Version: "dev", GoVersion: "go1.25.5", Platform: "windows/amd64"},
			want: "vario dev (commit unknown, built unknown, go1.25.5, windows/amd64)",
		},
	} {
		if got := tc.info.String(); got != tc.want {
			t.Errorf("%s:\n got %q\nwant %q", tc.name, got, tc.want)
		}
	}
}

func TestInjectedValuesTakePrecedence(t *testing.T) {
	defer func(v, c, d string) { Version, Commit, Date = v, c, d }(Version, Commit, Date)
	Version, Commit, Date = "v0.1.0-beta.1", "0123456789abcdef", "2026-11-13T12:00:00Z"
	info := Get()
	if info.Version != Version || info.Commit != Commit || info.Date != Date {
		t.Fatalf("Get() = %+v; want the injected values", info)
	}
	if info.Platform != runtime.GOOS+"/"+runtime.GOARCH || info.GoVersion != runtime.Version() {
		t.Fatalf("Get() = %+v; wrong platform or Go version", info)
	}
}

// TestReleaseLinkerFlags builds the CLI with the linker flags used by
// `make build` and scripts/dist.sh and checks what `vario version` reports.
func TestReleaseLinkerFlags(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the vario binary")
	}
	gobin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go toolchain not on PATH")
	}
	const (
		pkg     = "github.com/nedanwr/vario/internal/buildinfo"
		version = "v0.1.0-beta.1"
		commit  = "0123456789abcdef0123456789abcdef01234567"
		date    = "2026-11-13T12:00:00Z"
	)
	exe := filepath.Join(t.TempDir(), "vario")
	if runtime.GOOS == "windows" {
		exe += ".exe"
	}
	build := exec.Command(gobin, "build", "-buildvcs=false",
		"-ldflags", "-X "+pkg+".Version="+version+" -X "+pkg+".Commit="+commit+" -X "+pkg+".Date="+date,
		"-o", exe, "github.com/nedanwr/vario/cmd/vario")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}

	out, err := exec.Command(exe, "version").Output()
	if err != nil {
		t.Fatalf("vario version: %v", err)
	}
	want := "vario " + version + " (commit " + commit[:12] + ", built " + date + ", " + runtime.Version() + ", " + runtime.GOOS + "/" + runtime.GOARCH + ")\n"
	if string(out) != want {
		t.Fatalf("vario version:\n got %q\nwant %q", out, want)
	}

	out, err = exec.Command(exe, "version", "--short").Output()
	if err != nil {
		t.Fatalf("vario version --short: %v", err)
	}
	if strings.TrimSpace(string(out)) != version {
		t.Fatalf("vario version --short = %q; want %q", out, version)
	}
}
