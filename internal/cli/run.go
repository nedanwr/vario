package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/nedanwr/vario/internal/buildinfo"
	"github.com/nedanwr/vario/internal/collector"
	"github.com/nedanwr/vario/internal/privacy"
)

func (a *app) runCmd() *cobra.Command {
	var logFile string
	cmd := &cobra.Command{
		Use:   "run",
		Short: "Run the collector in the foreground",
		Long: `Run continuously synchronizes every enabled source into the local spool and
delivers spooled data to ClickHouse. Sources without a cursor are backfilled
first. Stop with Ctrl-C (SIGINT) or SIGTERM; pending data stays spooled and is
delivered on the next start. The background service runs this same command.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := a.loadConfig()
			if err != nil {
				return err
			}
			var w io.Writer = a.stderr
			if logFile != "" {
				rw, err := openRotating(logFile, 10<<20)
				if err != nil {
					return err
				}
				defer rw.Close()
				w = rw
			}
			log := a.logger(w, cfg.Collector.LogLevel)

			policy, err := privacy.New(cfg.Privacy)
			if err != nil {
				return withCode(ExitUsage, err)
			}
			srcs, _ := buildSources(cfg, policy, "")
			if len(srcs) == 0 {
				return withCode(ExitUsage, errors.New("no enabled sources; run `vario sources add t3code`"))
			}
			defer func() {
				for _, s := range srcs {
					s.Close()
				}
			}()
			client, err := newClient(cfg)
			if err != nil {
				return withCode(ExitUsage, err)
			}
			store, err := openStore(cfg)
			if err != nil {
				return err
			}
			defer store.Close()

			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			lctx, release, err := collector.Lease(ctx, store, "run", 45*time.Second)
			if err != nil {
				return err
			}
			defer release()

			log.Info("vario collector starting", "version", buildinfo.Version, "mode", policy.Mode(), "sources", len(srcs),
				"clickhouse", cfg.ClickHouse.URL, "database", cfg.ClickHouse.Database)
			c := newCollector(cfg, store, client, srcs, policy, log)
			err = c.Run(lctx)
			if cause := context.Cause(lctx); errors.Is(cause, collector.ErrLeaseLost) {
				log.Error("collector stopped", "error", cause.Error())
				return cause
			}
			log.Info("vario collector stopped")
			return err
		},
	}
	cmd.Flags().StringVar(&logFile, "log-file", "", "write logs to this file (rotated at 10 MiB) instead of stderr")
	return cmd
}

// rotatingFile is a minimal size-based log rotator keeping one previous file.
type rotatingFile struct {
	mu   sync.Mutex
	path string
	max  int64
	f    *os.File
	size int64
}

func openRotating(path string, max int64) (*rotatingFile, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	r := &rotatingFile{path: path, max: max}
	return r, r.open()
}

func (r *rotatingFile) open() error {
	f, err := os.OpenFile(r.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("open log file: %w", err)
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	r.f, r.size = f, st.Size()
	return nil
}

func (r *rotatingFile) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.size+int64(len(p)) > r.max {
		r.f.Close()
		_ = os.Rename(r.path, r.path+".1")
		if err := r.open(); err != nil {
			return 0, err
		}
	}
	n, err := r.f.Write(p)
	r.size += int64(n)
	return n, err
}

func (r *rotatingFile) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.f.Close()
}
