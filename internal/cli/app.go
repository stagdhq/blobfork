// Package cli defines the blobfork command tree.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/urfave/cli/v3"

	"github.com/stagdhq/blobfork/internal/telemetry"
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

// serve serves a fork until ctx ends, with OpenTelemetry on if an OTLP
// endpoint is set.
func serve(ctx context.Context, version, addr string, handler http.Handler, log *slog.Logger) error {
	stop, err := telemetry.Setup(ctx, version)
	if err != nil {
		return fmt.Errorf("opentelemetry: %w", err)
	}

	defer func() {
		flush, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		if err := stop(flush); err != nil {
			log.Warn("blobfork: opentelemetry", "err", err)
		}
	}()

	if telemetry.Enabled() {
		log.Info("blobfork: opentelemetry on")
	}

	server := &http.Server{
		Addr:              addr,
		Handler:           telemetry.Handler(handler),
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

// listenFlag is where blobfork serves the app, for serve and health.
func listenFlag(value string) *cli.StringFlag {
	return &cli.StringFlag{
		Name:    "listen",
		Usage:   "address the app reaches blobfork at",
		Value:   value,
		Sources: cli.EnvVars("BLOBFORK_LISTEN"),
	}
}

// healthCommand asks the blobfork serving at --listen whether it and its
// emulator are up, for a container's health check.
func healthCommand(emulator, listen, path string) *cli.Command {
	return &cli.Command{
		Name:  "health",
		Usage: "exit 0 when blobfork is serving and " + emulator + " answers, for a container's health check",
		Flags: []cli.Flag{
			listenFlag(listen),
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			host, port, err := net.SplitHostPort(cmd.String("listen"))
			if err != nil {
				return fmt.Errorf("--listen: %w", err)
			}

			// Listening on every address, blobfork answers on loopback.
			if host == "" || host == "0.0.0.0" || host == "::" {
				host = "127.0.0.1"
			}

			ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()

			req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+net.JoinHostPort(host, port)+path, nil)
			if err != nil {
				return err
			}

			res, err := http.DefaultClient.Do(req)
			if err != nil {
				return fmt.Errorf("not serving: %w", err)
			}
			defer res.Body.Close()

			body, _ := io.ReadAll(io.LimitReader(res.Body, 4<<10))

			if res.StatusCode != http.StatusOK {
				return fmt.Errorf("not ready: %s", strings.TrimSpace(string(body)))
			}

			return nil
		},
	}
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
