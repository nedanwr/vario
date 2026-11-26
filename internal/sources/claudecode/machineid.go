package claudecode

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/nedanwr/vario/internal/config"
)

// machineIDFile is where the machine identifier is kept once resolved,
// inside the Vario home directory.
const machineIDFile = "machine-id"

var (
	machineOnce sync.Once
	machineVal  string
)

// machineID returns a stable identifier of this machine, only ever hashed.
// The first resolution (the platform's machine identifier, falling back to
// the host name) is stored in the Vario home directory and reused from then
// on, so source identifiers, and the cursors keyed by them, survive host
// name changes and lookups that fail or time out.
func machineID() string {
	machineOnce.Do(func() {
		path := ""
		if home, err := config.HomeDir(); err == nil {
			path = filepath.Join(home, machineIDFile)
		}
		machineVal = loadMachineID(path, readMachineID)
	})
	return machineVal
}

// loadMachineID returns the identifier stored at path, or resolves one with
// read (falling back to the host name) and stores it there. An identifier
// that cannot be stored is resolved again by the next run.
func loadMachineID(path string, read func() string) string {
	if id := readStoredID(path); id != "" {
		return id
	}
	id := read()
	if id == "" {
		id, _ = os.Hostname()
	}
	if path == "" || id == "" || os.MkdirAll(filepath.Dir(path), 0o700) != nil {
		return id
	}
	f, err := os.CreateTemp(filepath.Dir(path), machineIDFile+".*")
	if err != nil {
		return id
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	_, werr := f.WriteString(id + "\n")
	if cerr := f.Close(); werr != nil || cerr != nil {
		return id
	}
	// Link instead of rename so a concurrently stored identifier is never
	// replaced; whichever process stored first wins for everyone.
	if os.Link(tmp, path) != nil {
		if stored := readStoredID(path); stored != "" {
			return stored
		}
	}
	return id
}

func readStoredID(path string) string {
	if path == "" {
		return ""
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

var (
	ioregRe = regexp.MustCompile(`"IOPlatformUUID"\s*=\s*"([^"]+)"`)
	regRe   = regexp.MustCompile(`MachineGuid\s+REG_SZ\s+(\S+)`)
)

func readMachineID() string {
	switch runtime.GOOS {
	case "darwin":
		if out := run("/usr/sbin/ioreg", "-rd1", "-c", "IOPlatformExpertDevice"); out != "" {
			if m := ioregRe.FindStringSubmatch(out); m != nil {
				return m[1]
			}
		}
	case "windows":
		reg := filepath.Join(os.Getenv("SystemRoot"), "System32", "reg.exe")
		if out := run(reg, "query", `HKLM\SOFTWARE\Microsoft\Cryptography`, "/v", "MachineGuid"); out != "" {
			if m := regRe.FindStringSubmatch(out); m != nil {
				return m[1]
			}
		}
	default:
		for _, p := range []string{"/etc/machine-id", "/var/lib/dbus/machine-id"} {
			if b, err := os.ReadFile(p); err == nil {
				if id := strings.TrimSpace(string(b)); id != "" {
					return id
				}
			}
		}
	}
	return ""
}

func run(name string, args ...string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).Output()
	if err != nil {
		return ""
	}
	return string(out)
}
