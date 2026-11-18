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
)

var (
	machineOnce sync.Once
	machineVal  string
)

// machineID returns a stable identifier of this machine (it survives host
// name changes), falling back to the host name. It is only ever hashed.
func machineID() string {
	machineOnce.Do(func() {
		machineVal = readMachineID()
		if machineVal == "" {
			machineVal, _ = os.Hostname()
		}
	})
	return machineVal
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
