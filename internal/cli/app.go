// Package cli defines the blobfork command tree.
package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/urfave/cli/v3"

	"github.com/stagdhq/blobfork/internal/azure"
)

// AzuriteKey is Azurite's well-known account key, the default for the
// account the app uses: the fork is the app's own, nothing in it is secret.
const AzuriteKey = "Eby8vdM02xNOcqFlqUwJPLlmEtlCDXJ1OUzFT50uSRZ6IFsuFq2UVErCz4I6tq/K1SZFPTOtr/KBHBeksoGMGw=="

// New returns the root command.
func New(version string) *cli.Command {
	return &cli.Command{
		Name:    "blobfork",
		Usage:   "a copy-on-write fork of a blob container",
		Version: version,
		Commands: []*cli.Command{
			serveCommand(version),
			checkCommand(),
			versionCommand(version),
		},
	}
}

// fromFlag is the real container, for serve and check.
func fromFlag() *cli.StringFlag {
	return &cli.StringFlag{
		Name:     "from",
		Usage:    "the real container's URL, with a SAS that can read and list (https://<account>.blob.core.windows.net/<container>?sv=…&sp=rl&sig=…)",
		Sources:  cli.EnvVars("BLOBFORK_FROM"),
		Required: true,
	}
}

func serveCommand(version string) *cli.Command {
	return &cli.Command{
		Name:  "serve",
		Usage: "serve the fork: the app's blob endpoint, in front of Azurite",
		Description: "Requests pass through to Azurite. Reads the fork can't answer come from the real\n" +
			"container, deletes of its blobs leave a marker, listings merge both, and copies\n" +
			"from it copy the blob up first. The real container is never written.",
		Flags: []cli.Flag{
			fromFlag(),
			&cli.StringFlag{
				Name:    "listen",
				Usage:   "address the app reaches blobfork at",
				Value:   ":10000",
				Sources: cli.EnvVars("BLOBFORK_LISTEN"),
			},
			&cli.StringFlag{
				Name:    "azurite",
				Usage:   "Azurite's blob endpoint, without the account",
				Value:   "http://127.0.0.1:10010",
				Sources: cli.EnvVars("BLOBFORK_AZURITE"),
			},
			&cli.StringFlag{
				Name:    "account",
				Usage:   "the account in Azurite the app uses (AccountName in its connection string)",
				Value:   "blobfork",
				Sources: cli.EnvVars("BLOBFORK_ACCOUNT"),
			},
			&cli.StringFlag{
				Name:        "key",
				Usage:       "the account's key (AccountKey); Azurite's well-known key if not set",
				Sources:     cli.EnvVars("BLOBFORK_KEY"),
				DefaultText: "Azurite's",
			},
			&cli.StringFlag{
				Name:        "container",
				Usage:       "the forked container's name, if not the real one's",
				Sources:     cli.EnvVars("BLOBFORK_CONTAINER"),
				DefaultText: "the real one's",
			},
			&cli.StringFlag{
				Name:    "log-level",
				Usage:   "debug, info, warn or error",
				Value:   "info",
				Sources: cli.EnvVars("BLOBFORK_LOG_LEVEL"),
			},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			var level slog.Level

			if err := level.UnmarshalText([]byte(cmd.String("log-level"))); err != nil {
				return fmt.Errorf("--log-level: %w", err)
			}

			log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

			lower, err := azure.NewSASLayer(cmd.String("from"))
			if err != nil {
				return fmt.Errorf("--from: %w", err)
			}

			key := cmd.String("key")
			if key == "" {
				key = AzuriteKey
			}

			name := cmd.String("container")
			if name == "" {
				name = lower.Container()
			}

			fork, err := azure.New(ctx, azure.Config{
				Azurite:   cmd.String("azurite"),
				Account:   cmd.String("account"),
				Key:       key,
				Container: name,
				Lower:     lower,
				Log:       log,
			})
			if err != nil {
				return err
			}

			server := &http.Server{
				Addr:              cmd.String("listen"),
				Handler:           fork,
				ReadHeaderTimeout: 30 * time.Second,
			}

			go func() {
				<-ctx.Done()

				shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()

				server.Shutdown(shutdown)
			}()

			log.Info("blobfork: listening", "addr", server.Addr, "version", version, "from", lower.URL, "container", name)

			if err := server.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
				return err
			}

			return nil
		},
	}
}

// writePermissions are the SAS permissions that could change the real
// container.
const writePermissions = "acwdxytmei"

func checkCommand() *cli.Command {
	return &cli.Command{
		Name:  "check",
		Usage: "check that the real container can be listed and read, and that the SAS can't write",
		Flags: []cli.Flag{
			fromFlag(),
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			out := cmd.Root().Writer

			lower, err := azure.NewSASLayer(cmd.String("from"))
			if err != nil {
				return fmt.Errorf("--from: %w", err)
			}

			raw, _ := url.Parse(cmd.String("from"))

			if perms := raw.Query().Get("sp"); strings.ContainsAny(perms, writePermissions) {
				fmt.Fprintf(out, "warning: the SAS may change the container (sp=%s); blobfork only needs r and l\n", perms)
			}

			if expiry := raw.Query().Get("se"); expiry != "" {
				fmt.Fprintf(out, "SAS expires %s\n", expiry)
			}

			page, err := lower.List(ctx, azure.ListQuery{MaxResults: 10})
			if err != nil {
				return fmt.Errorf("listing %s: %w", lower.URL, err)
			}

			more := ""
			if page.NextMarker != "" {
				more = ", more after"
			}

			fmt.Fprintf(out, "listed %s: %d blobs on the first page%s\n", lower.URL, len(page.Entries), more)

			if len(page.Entries) == 0 {
				return nil
			}

			name := page.Entries[0].Name

			res, err := lower.Blob(ctx, http.MethodHead, name, "", http.Header{})
			if err != nil {
				return fmt.Errorf("reading %s: %w", name, err)
			}

			res.Body.Close()

			if res.StatusCode != http.StatusOK {
				return fmt.Errorf("reading %s: %s", name, res.Status)
			}

			fmt.Fprintf(out, "read %s: %s bytes\n", name, res.Header.Get("Content-Length"))

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
