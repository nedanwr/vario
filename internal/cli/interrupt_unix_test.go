//go:build !windows

package cli

import (
	"bytes"
	"io"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/nedanwr/vario/internal/sources/t3code/t3codetest"
)

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// interruptAt runs init, feeds the answers, waits until the output contains
// waitFor and sends SIGINT to the process, like Ctrl-C in a terminal. It
// returns the output and the exit status the interrupt handler chose.
func interruptAt(t *testing.T, waitFor string, args []string, a ...string) (string, int) {
	t.Helper()
	exited := make(chan int, 1)
	prev := exitProcess
	exitProcess = func(code int) { exited <- code }
	t.Cleanup(func() { exitProcess = prev })

	pr, pw := io.Pipe()
	out := &syncBuffer{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		run(args, pr, out, out)
	}()
	go func() { _, _ = io.WriteString(pw, answers(a...)) }()
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(out.String(), waitFor) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %q:\n%s", waitFor, out.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond) // let the wizard block on the next prompt
	if err := syscall.Kill(syscall.Getpid(), syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	var code int
	select {
	case code = <-exited:
	case <-time.After(10 * time.Second):
		t.Fatalf("the interrupt was not handled:\n%s", out.String())
	}
	text := out.String()
	pw.Close() // the stubbed exit returns; end the wizard
	<-done
	return text, code
}

func TestCtrlCBeforeChangesSavesNothing(t *testing.T) {
	cfgPath, _ := isolate(t)
	ch, url := guidedFake(t)
	a := guidedUntilApplied(url)
	out, code := interruptAt(t, "Cancel setup (nothing is changed)", []string{"init"}, a[:len(a)-1]...)
	if code != 130 || !strings.Contains(out, "Setup interrupted.\nNothing was saved.") {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	if len(ch.changes()) != 0 || fileExists(pendingPath(cfgPath)) || fileExists(cfgPath) {
		t.Fatal("an interrupt before applying must leave nothing behind")
	}
}

func TestCtrlCAfterUserCreatedKeepsPassword(t *testing.T) {
	cfgPath, _ := isolate(t)
	fix := t3codetest.New(t)
	ch, url := guidedFake(t)
	out, code := interruptAt(t, "Save this configuration", []string{"init", "--t3code-path", fix.Path},
		append(guidedUntilApplied(url), "", "", "")...)
	if code != 130 {
		t.Fatalf("exit %d", code)
	}
	for _, want := range []string{"Setup interrupted.", "ClickHouse user vario was created at", "Its password is kept in", "Run `vario init` again to finish"} {
		if !strings.Contains(out, want) {
			t.Errorf("interrupt notice missing %q:\n%s", want, out)
		}
	}
	p := readPending(t, cfgPath)
	if len(p) != 1 || !authenticates(ch, "vario", p[0].Password) {
		t.Fatalf("the password must be on disk: %+v", p)
	}
	if strings.Contains(out, p[0].Password) {
		t.Fatal("the password must not be printed")
	}
}
