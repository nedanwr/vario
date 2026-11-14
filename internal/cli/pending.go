package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/nedanwr/vario/internal/config"
)

// pendingCredentials is the password of a ClickHouse user that guided setup
// created (or is about to create) and that no saved configuration uses yet.
//
// Entries are written to a file next to the configuration (mode 0600) before
// CREATE USER is sent, and removed only after a configuration using the
// password has been saved. The password therefore survives Ctrl-C, closed
// input, a cancelled or failed setup, a crash or SIGKILL; the next `vario
// init` offers to finish the setup with it. The file never contains
// administrative credentials.
type pendingCredentials struct {
	Endpoint  string    `json:"endpoint"` // the URL as configured
	Database  string    `json:"database"`
	User      string    `json:"user"`
	Password  string    `json:"password"`
	Generated bool      `json:"generated"`
	Created   time.Time `json:"created"`
}

func (p pendingCredentials) String() string {
	return fmt.Sprintf("user %s at %s (database %s, %s)", p.User, p.Endpoint, p.Database, p.Created.Local().Format("2006-01-02 15:04"))
}

// is reports whether p is the given user at the given endpoint.
func (p pendingCredentials) is(endpoint, user string) bool {
	return config.NormalizeEndpoint(p.Endpoint) == config.NormalizeEndpoint(endpoint) && p.User == user
}

const pendingFileName = "pending-credentials.json"

func pendingPath(cfgPath string) string {
	return filepath.Join(filepath.Dir(cfgPath), pendingFileName)
}

// pendingStore is the set of pending credentials in one file.
type pendingStore struct {
	path  string
	Users []pendingCredentials `json:"users"`
}

// loadPendingStore reads the pending credentials file (empty if it does not
// exist). An unreadable file is reported and never overwritten, since it may
// hold the only copy of a password.
func loadPendingStore(path string) (*pendingStore, error) {
	s := &pendingStore{path: path}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", config.DisplayPath(path), err)
	}
	if err := json.Unmarshal(b, s); err != nil {
		return nil, fmt.Errorf("%s cannot be parsed (%v); it may hold the password of a ClickHouse user created by an interrupted `vario init`, so it was left untouched", config.DisplayPath(path), err)
	}
	return s, nil
}

func (s *pendingStore) find(endpoint, user string) *pendingCredentials {
	for i := range s.Users {
		if s.Users[i].is(endpoint, user) {
			return &s.Users[i]
		}
	}
	return nil
}

// put adds or replaces the entry for p's endpoint and user and writes the file.
func (s *pendingStore) put(p pendingCredentials) error {
	next := []pendingCredentials{p}
	for _, u := range s.Users {
		if !u.is(p.Endpoint, p.User) {
			next = append(next, u)
		}
	}
	if err := s.write(next); err != nil {
		return err
	}
	s.Users = next
	return nil
}

// drop removes the entry for endpoint and user; the file is deleted when it
// becomes empty.
func (s *pendingStore) drop(endpoint, user string) error {
	var next []pendingCredentials
	for _, u := range s.Users {
		if !u.is(endpoint, user) {
			next = append(next, u)
		}
	}
	if len(next) == len(s.Users) {
		return nil
	}
	if err := s.write(next); err != nil {
		return err
	}
	s.Users = next
	return nil
}

func (s *pendingStore) write(users []pendingCredentials) error {
	if len(users) == 0 {
		err := os.Remove(s.path)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	b, err := json.MarshalIndent(struct {
		Note  string               `json:"note"`
		Users []pendingCredentials `json:"users"`
	}{"Passwords of ClickHouse users created by `vario init` that no saved configuration uses yet. Run `vario init` to finish the setup; it removes entries once they are saved.", users}, "", "  ")
	if err != nil {
		return err
	}
	return config.WritePrivateFile(s.path, append(b, '\n'))
}
