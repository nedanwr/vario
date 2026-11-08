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
		return readHidden(f, p.w)
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

// readHidden reads a password from the terminal with echo disabled. If the
// user presses Ctrl-C, the terminal state is restored before exiting, since
// it would otherwise be left without echo. Nothing has been saved at that point.
func readHidden(f *os.File, w io.Writer) (string, error) {
	if st, err := term.GetState(int(f.Fd())); err == nil {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
		done := make(chan struct{})
		defer func() { signal.Stop(sig); close(done) }()
		go func() {
			select {
			case <-sig:
				_ = term.Restore(int(f.Fd()), st)
				fmt.Fprintln(w, "\nCancelled; nothing was saved.")
				os.Exit(130)
			case <-done:
			}
		}()
	}
	b, err := term.ReadPassword(int(f.Fd()))
	fmt.Fprintln(w)
	return string(b), err
}

func isTerminal(r io.Reader) bool {
	f, ok := r.(*os.File)
	return ok && term.IsTerminal(int(f.Fd()))
}
