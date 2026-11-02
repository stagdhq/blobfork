package cli

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/urfave/cli/v3"

	"github.com/stagdhq/blobfork/internal/azure"
)

// AzuriteKey is Azurite's well-known account key, the default for the
// account the app uses: the fork is the app's own, nothing in it is secret.
const AzuriteKey = "Eby8vdM02xNOcqFlqUwJPLlmEtlCDXJ1OUzFT50uSRZ6IFsuFq2UVErCz4I6tq/K1SZFPTOtr/KBHBeksoGMGw=="

// azureCommand is the Azure front: a fork of a container, in front of
// Azurite.
func azureCommand(version string) *cli.Command {
	return &cli.Command{
		Name:  "azure",
		Usage: "fork an Azure Blob Storage container, in front of Azurite",
		Commands: []*cli.Command{
			azureServeCommand(version),
			azureCheckCommand(),
		},
	}
}

// azureFromFlag is the real container, for serve and check.
func azureFromFlag() *cli.StringFlag {
	return &cli.StringFlag{
		Name:     "from",
		Usage:    "the real container's URL, with a SAS that can read and list (https://<account>.blob.core.windows.net/<container>?sv=…&sp=rl&sig=…)",
		Sources:  cli.EnvVars("BLOBFORK_FROM"),
		Required: true,
	}
}

func azureServeCommand(version string) *cli.Command {
	return &cli.Command{
		Name:  "serve",
		Usage: "serve the fork: the app's blob endpoint, in front of Azurite",
		Description: "Requests pass through to Azurite. Reads the fork can't answer come from the real\n" +
			"container, deletes of its blobs leave a marker, listings merge both, and copies\n" +
			"from it copy the blob up first. The real container is never written.",
		Flags: []cli.Flag{
			azureFromFlag(),
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
			logFlag(),
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			log, err := logger(cmd)
			if err != nil {
				return err
			}

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

			log.Info("blobfork: serving", "version", version, "from", lower.URL, "container", name)

			return serve(ctx, cmd.String("listen"), fork, log)
		},
	}
}

// writePermissions are the SAS permissions that could change the real
// container.
const writePermissions = "acwdxytmei"

func azureCheckCommand() *cli.Command {
	return &cli.Command{
		Name:  "check",
		Usage: "check that the real container can be listed and read, and that the SAS can't write",
		Flags: []cli.Flag{
			azureFromFlag(),
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
