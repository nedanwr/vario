package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"golang.org/x/term"
)

// errInputEnded is returned when stdin is closed in the middle of a prompt.
var errInputEnded = errors.New("input ended before setup was complete; nothing was saved")

type prompter struct {
	r           *bufio.Reader
	w           io.Writer
	in          io.Reader
	interactive bool

	// Interrupt handling (see trapInterrupts).
	mu       sync.Mutex
	trapped  bool
	notice   string
	hidden   *term.State // terminal state to restore while echo is off
	hiddenFd int
}

func newPrompter(in io.Reader, w io.Writer, interactive bool) *prompter {
	return &prompter{r: bufio.NewReader(in), w: w, in: in, interactive: interactive}
}

func (p *prompter) readLine() (string, error) {
	line, err := p.r.ReadString('\n')
	if err != nil {
		if errors.Is(err, io.EOF) && line != "" {
			return strings.TrimSpace(line), nil
		}
		if errors.Is(err, io.EOF) {
			fmt.Fprintln(p.w)
			return "", errInputEnded
		}
		return "", fmt.Errorf("read input: %w", err)
	}
	return strings.TrimSpace(line), nil
}

// ask prompts for a value; an empty answer selects def.
func (p *prompter) ask(label, def string) (string, error) {
	if !p.interactive {
		return def, nil
	}
	if def != "" {
		fmt.Fprintf(p.w, "%s [%s]: ", label, def)
	} else {
		fmt.Fprintf(p.w, "%s: ", label)
	}
	line, err := p.readLine()
	if err != nil {
		return "", err
	}
	if line == "" {
		return def, nil
	}
	return line, nil
}

// askValid repeats the prompt until check accepts the answer.
func (p *prompter) askValid(label, def string, check func(string) error) (string, error) {
	for {
		v, err := p.ask(label, def)
		if err != nil {
			return "", err
		}
		if err := check(v); err != nil {
			if !p.interactive {
				return "", err
			}
			fmt.Fprintf(p.w, "  Invalid: %v.\n", err)
			continue
		}
		return v, nil
	}
}

func (p *prompter) confirm(label string, def bool) (bool, error) {
	if !p.interactive {
		return def, nil
	}
	d := "y/N"
	if def {
		d = "Y/n"
	}
	for {
		fmt.Fprintf(p.w, "%s [%s]: ", label, d)
		ans, err := p.readLine()
		if err != nil {
			return false, err
		}
		switch strings.ToLower(ans) {
		case "":
			return def, nil
		case "y", "yes":
			return true, nil
		case "n", "no":
			return false, nil
		}
		fmt.Fprintln(p.w, "  Please answer y or n.")
	}
}

// choose shows numbered options and returns the selected index.
func (p *prompter) choose(label string, options []string, def int) (int, error) {
	if !p.interactive {
		return def, nil
	}
	if label != "" {
		fmt.Fprintln(p.w, label)
	}
	for i, o := range options {
		mark := " "
		if i == def {
			mark = "❯"
		}
		fmt.Fprintf(p.w, "  %s %d) %s\n", mark, i+1, o)
	}
	for {
		fmt.Fprintf(p.w, "Choose 1-%d [%d]: ", len(options), def+1)
		ans, err := p.readLine()
		if err != nil {
			return 0, err
		}
		if ans == "" {
			return def, nil
		}
		if n, err := strconv.Atoi(ans); err == nil && n >= 1 && n <= len(options) {
			return n - 1, nil
		}
		fmt.Fprintf(p.w, "  Enter a number between 1 and %d.\n", len(options))
	}
}

// secret reads a line without echo when stdin is a terminal.
func (p *prompter) secret(label string) (string, error) {
	if !p.interactive {
		return "", nil
	}
	fmt.Fprintf(p.w, "%s: ", label)
	if f, ok := p.in.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		return p.readHidden(f)
	}
	line, err := p.r.ReadString('\n')
	if err != nil && !(errors.Is(err, io.EOF) && line != "") {
		if errors.Is(err, io.EOF) {
			return "", errInputEnded
		}
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

// exitProcess ends the process after an interrupt; tests replace it.
var exitProcess = os.Exit

// trapInterrupts handles Ctrl-C (SIGINT) and SIGTERM while a command prompts:
// the terminal is restored if a hidden prompt disabled echo, the notice set
// with setNotice explains what was (and was not) saved, and the process exits
// with status 130. The returned function removes the handler. A process
// killed with SIGKILL, or a crash, runs no handler: anything that must
// survive an interruption has to be on disk before the step that needs it
// (see pendingCredentials).
func (p *prompter) trapInterrupts(notice string) (stop func()) {
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	done := make(chan struct{})
	p.mu.Lock()
	p.trapped, p.notice = true, notice
	p.mu.Unlock()
	go func() {
		select {
		case <-sig:
			p.interrupted()
		case <-done:
		}
	}()
	return func() {
		signal.Stop(sig)
		close(done)
		p.mu.Lock()
		p.trapped, p.notice = false, ""
		p.mu.Unlock()
	}
}

// interrupted restores the terminal, explains the state and exits.
func (p *prompter) interrupted() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.hidden != nil {
		_ = term.Restore(p.hiddenFd, p.hidden)
		p.hidden = nil
	}
	fmt.Fprintln(p.w)
	if p.notice != "" {
		fmt.Fprint(p.w, p.notice)
	} else {
		fmt.Fprintln(p.w, "Cancelled; nothing was saved.")
	}
	exitProcess(130)
}

// setNotice replaces the text printed if the command is interrupted. The
// text is prepared in advance because the interrupt arrives on another
// goroutine while the command waits for input.
func (p *prompter) setNotice(s string) {
	p.mu.Lock()
	p.notice = s
	p.mu.Unlock()
}

// readHidden reads a password from the terminal with echo disabled. The
// terminal state is recorded so that an interrupt restores echo before the
// process exits.
func (p *prompter) readHidden(f *os.File) (string, error) {
	fd := int(f.Fd())
	if st, err := term.GetState(fd); err == nil {
		p.mu.Lock()
		p.hidden, p.hiddenFd = st, fd
		trapped := p.trapped
		p.mu.Unlock()
		if !trapped {
			defer p.trapInterrupts("")()
		}
		defer func() {
			p.mu.Lock()
			p.hidden = nil
			p.mu.Unlock()
		}()
	}
	b, err := term.ReadPassword(fd)
	fmt.Fprintln(p.w)
	return string(b), err
}

func isTerminal(r io.Reader) bool {
	f, ok := r.(*os.File)
	return ok && term.IsTerminal(int(f.Fd()))
}
