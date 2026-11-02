package cli

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/urfave/cli/v3"

	"github.com/stagdhq/blobfork/internal/azure"
)

// AzuriteKey is Azurite's well-known account key, the default for the
// account the app uses: the fork is the app's own, nothing in it is secret.
const AzuriteKey = "Eby8vdM02xNOcqFlqUwJPLlmEtlCDXJ1OUzFT50uSRZ6IFsuFq2UVErCz4I6tq/K1SZFPTOtr/KBHBeksoGMGw=="

// azureCommand is the Azure front: forks of containers, in front of
// Azurite.
func azureCommand(version string) *cli.Command {
	return &cli.Command{
		Name:  "azure",
		Usage: "fork Azure Blob Storage containers, in front of Azurite",
		Commands: []*cli.Command{
			azureServeCommand(version),
			azureCheckCommand(),
		},
	}
}

// azureFromFlag is the real containers, for serve and check.
func azureFromFlag() cli.Flag {
	return fromFlag("a real container's URL, with a SAS that can read and list (https://<account>.blob.core.windows.net/<container>?sv=…&sp=rl&sig=…)")
}

// azureLowers are the real containers by the names of their forks.
func azureLowers(cmd *cli.Command) (map[string]*azure.SASLayer, error) {
	list, err := froms(cmd)
	if err != nil {
		return nil, err
	}

	lowers := map[string]*azure.SASLayer{}

	for _, f := range list {
		lower, err := azure.NewSASLayer(f.raw)
		if err != nil {
			return nil, fmt.Errorf("--from: %w", err)
		}

		name := f.name
		if name == "" {
			name = lower.Container()
		}

		if _, ok := lowers[name]; ok {
			return nil, fmt.Errorf("--from: %s twice", name)
		}

		lowers[name] = lower
	}

	return lowers, nil
}

func azureServeCommand(version string) *cli.Command {
	return &cli.Command{
		Name:  "serve",
		Usage: "serve the fork: the app's blob endpoint, in front of Azurite",
		Description: "Requests pass through to Azurite. Reads the fork can't answer come from the real\n" +
			"container, deletes of its blobs leave a marker, listings merge both, and copies\n" +
			"from it copy the blob up first. The real containers are never written.",
		DisableSliceFlagSeparator: true,
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
			logFlag(),
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			log, err := logger(cmd)
			if err != nil {
				return err
			}

			lowers, err := azureLowers(cmd)
			if err != nil {
				return err
			}

			key := cmd.String("key")
			if key == "" {
				key = AzuriteKey
			}

			containers := map[string]azure.Layer{}

			for name, lower := range lowers {
				containers[name] = lower

				log.Info("blobfork: forking", "container", name, "from", lower.URL)
			}

			fork, err := azure.New(ctx, azure.Config{
				Azurite:    cmd.String("azurite"),
				Account:    cmd.String("account"),
				Key:        key,
				Containers: containers,
				Log:        log,
			})
			if err != nil {
				return err
			}

			log.Info("blobfork: serving", "version", version)

			return serve(ctx, cmd.String("listen"), fork, log)
		},
	}
}

// writePermissions are the SAS permissions that could change the real
// container.
const writePermissions = "acwdxytmei"

func azureCheckCommand() *cli.Command {
	return &cli.Command{
		Name:                      "check",
		Usage:                     "check that the real containers can be listed and read, and that their SAS can't write",
		DisableSliceFlagSeparator: true,
		Flags: []cli.Flag{
			azureFromFlag(),
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			out := cmd.Root().Writer

			list, err := froms(cmd)
			if err != nil {
				return err
			}

			for _, f := range list {
				if err := azureCheck(ctx, out, f.raw); err != nil {
					return err
				}
			}

			return nil
		},
	}
}

// azureCheck lists a real container, reads a blob of it, and says whether
// its SAS could write.
func azureCheck(ctx context.Context, out io.Writer, from string) error {
	lower, err := azure.NewSASLayer(from)
	if err != nil {
		return fmt.Errorf("--from: %w", err)
	}

	raw, _ := url.Parse(from)

	if perms := raw.Query().Get("sp"); strings.ContainsAny(perms, writePermissions) {
		fmt.Fprintf(out, "warning: the SAS for %s may change the container (sp=%s); blobfork only needs r and l\n", lower.URL, perms)
	}

	if expiry := raw.Query().Get("se"); expiry != "" {
		fmt.Fprintf(out, "SAS for %s expires %s\n", lower.URL, expiry)
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
}
