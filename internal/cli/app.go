// Package cli defines the blobfork command tree.
package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"runtime"
	"time"

	"github.com/urfave/cli/v3"
)

// New returns the root command.
func New(version string) *cli.Command {
	return &cli.Command{
		Name:    "blobfork",
		Usage:   "a copy-on-write fork of a blob container or bucket",
		Version: version,
		Commands: []*cli.Command{
			azureCommand(version),
			s3Command(version),
			versionCommand(version),
		},
	}
}

// logFlag sets how much serve logs.
func logFlag() *cli.StringFlag {
	return &cli.StringFlag{
		Name:    "log-level",
		Usage:   "debug, info, warn or error",
		Value:   "info",
		Sources: cli.EnvVars("BLOBFORK_LOG_LEVEL"),
	}
}

func logger(cmd *cli.Command) (*slog.Logger, error) {
	var level slog.Level

	if err := level.UnmarshalText([]byte(cmd.String("log-level"))); err != nil {
		return nil, fmt.Errorf("--log-level: %w", err)
	}

	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})), nil
}

// serve serves a fork until ctx ends.
func serve(ctx context.Context, addr string, handler http.Handler, log *slog.Logger) error {
	server := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 30 * time.Second,
	}

	go func() {
		<-ctx.Done()

		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		server.Shutdown(shutdown)
	}()

	log.Info("blobfork: listening", "addr", addr)

	if err := server.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}

	return nil
}

func versionCommand(version string) *cli.Command {
	return &cli.Command{
		Name:  "version",
		Usage: "print blobfork's version",
		Action: func(_ context.Context, cmd *cli.Command) error {
			_, err := fmt.Fprintf(cmd.Root().Writer, "blobfork %s (%s/%s)\n", version, runtime.GOOS, runtime.GOARCH)

			return err
		},
	}
}
