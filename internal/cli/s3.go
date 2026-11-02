package cli

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/urfave/cli/v3"

	"github.com/stagdhq/blobfork/internal/s3"
)

// s3Command is the S3 front: forks of buckets, in front of Floci.
func s3Command(version string) *cli.Command {
	return &cli.Command{
		Name:  "s3",
		Usage: "fork S3 buckets (AWS, R2, MinIO, …), in front of Floci",
		Commands: []*cli.Command{
			s3ServeCommand(version),
			s3CheckCommand(),
		},
	}
}

// s3FromFlags are the real buckets and how to reach them, for serve and
// check: all at the one endpoint, read with the one key.
func s3FromFlags() []cli.Flag {
	return []cli.Flag{
		fromFlag("a real bucket, s3://<bucket>"),
		&cli.StringFlag{
			Name:        "from-endpoint",
			Usage:       "the real buckets' S3 endpoint, for R2, MinIO and the like",
			Sources:     cli.EnvVars("BLOBFORK_FROM_ENDPOINT"),
			DefaultText: "AWS's, https://s3.<region>.amazonaws.com",
		},
		&cli.StringFlag{
			Name:    "from-region",
			Usage:   "the real buckets' region",
			Value:   "us-east-1",
			Sources: cli.EnvVars("BLOBFORK_FROM_REGION"),
		},
		&cli.BoolFlag{
			Name:        "from-path-style",
			Usage:       "address the real buckets in the path, not the host",
			Sources:     cli.EnvVars("BLOBFORK_FROM_PATH_STYLE"),
			DefaultText: "with --from-endpoint",
		},
		&cli.StringFlag{
			Name:     "from-access-key-id",
			Usage:    "an access key that can read and list the real buckets, and nothing more",
			Sources:  cli.EnvVars("BLOBFORK_FROM_ACCESS_KEY_ID"),
			Required: true,
		},
		&cli.StringFlag{
			Name:     "from-secret-access-key",
			Usage:    "its secret",
			Sources:  cli.EnvVars("BLOBFORK_FROM_SECRET_ACCESS_KEY"),
			Required: true,
		},
		&cli.StringFlag{
			Name:    "from-session-token",
			Usage:   "its session token, for temporary credentials",
			Sources: cli.EnvVars("BLOBFORK_FROM_SESSION_TOKEN"),
		},
	}
}

// s3Lowers are the real buckets by the names of their forks, from the
// flags.
func s3Lowers(cmd *cli.Command) (map[string]*s3.Bucket, error) {
	list, err := froms(cmd)
	if err != nil {
		return nil, err
	}

	region := cmd.String("from-region")

	endpoint := cmd.String("from-endpoint")
	pathStyle := cmd.Bool("from-path-style")

	if endpoint == "" {
		endpoint = "https://s3." + region + ".amazonaws.com"
	} else if !cmd.IsSet("from-path-style") {
		pathStyle = true
	}

	credentials := aws.Credentials{
		AccessKeyID:     cmd.String("from-access-key-id"),
		SecretAccessKey: cmd.String("from-secret-access-key"),
		SessionToken:    cmd.String("from-session-token"),
	}

	lowers := map[string]*s3.Bucket{}

	for _, f := range list {
		u, err := url.Parse(f.raw)
		if err != nil || u.Scheme != "s3" || u.Host == "" || strings.Trim(u.Path, "/") != "" {
			return nil, fmt.Errorf("--from: not a bucket, s3://<bucket>: %s", f.raw)
		}

		name := f.name
		if name == "" {
			name = u.Host
		}

		if _, ok := lowers[name]; ok {
			return nil, fmt.Errorf("--from: %s twice", name)
		}

		lowers[name] = &s3.Bucket{
			Endpoint:    endpoint,
			Name:        u.Host,
			Region:      region,
			PathStyle:   pathStyle,
			Credentials: credentials,
		}
	}

	return lowers, nil
}

func s3ServeCommand(version string) *cli.Command {
	return &cli.Command{
		Name:  "serve",
		Usage: "serve the fork: the app's S3 endpoint, in front of Floci",
		Description: "Requests pass through to Floci, which must check signatures\n" +
			"(FLOCI_AUTH_VALIDATE_SIGNATURES=true). Reads the fork can't answer come from the\n" +
			"real bucket, deletes of its objects leave a marker, listings merge both, and copies\n" +
			"from it copy the object up first. The real buckets are never written.",
		DisableSliceFlagSeparator: true,
		Flags: append(s3FromFlags(),
			&cli.StringFlag{
				Name:    "listen",
				Usage:   "address the app reaches blobfork at",
				Value:   ":4566",
				Sources: cli.EnvVars("BLOBFORK_LISTEN"),
			},
			&cli.StringFlag{
				Name:    "floci",
				Usage:   "Floci's endpoint",
				Value:   "http://127.0.0.1:4567",
				Sources: cli.EnvVars("BLOBFORK_FLOCI"),
			},
			&cli.StringFlag{
				Name:    "access-key-id",
				Usage:   "the access key the app signs with, Floci's",
				Value:   "test",
				Sources: cli.EnvVars("BLOBFORK_ACCESS_KEY_ID"),
			},
			&cli.StringFlag{
				Name:    "secret-access-key",
				Usage:   "its secret",
				Value:   "test",
				Sources: cli.EnvVars("BLOBFORK_SECRET_ACCESS_KEY"),
			},
			&cli.StringFlag{
				Name:    "region",
				Usage:   "the region the app signs for",
				Value:   "us-east-1",
				Sources: cli.EnvVars("BLOBFORK_REGION"),
			},
			logFlag(),
		),
		Action: func(ctx context.Context, cmd *cli.Command) error {
			log, err := logger(cmd)
			if err != nil {
				return err
			}

			lowers, err := s3Lowers(cmd)
			if err != nil {
				return err
			}

			buckets := map[string]s3.Layer{}

			for name, lower := range lowers {
				buckets[name] = lower

				log.Info("blobfork: forking", "bucket", name, "from", "s3://"+lower.Name, "endpoint", lower.Endpoint)
			}

			fork, err := s3.New(ctx, s3.Config{
				Floci: cmd.String("floci"),
				Credentials: aws.Credentials{
					AccessKeyID:     cmd.String("access-key-id"),
					SecretAccessKey: cmd.String("secret-access-key"),
				},
				Region:  cmd.String("region"),
				Buckets: buckets,
				Log:     log,
			})
			if err != nil {
				return err
			}

			log.Info("blobfork: serving", "version", version)

			return serve(ctx, cmd.String("listen"), fork, log)
		},
	}
}

func s3CheckCommand() *cli.Command {
	return &cli.Command{
		Name:                      "check",
		Usage:                     "check that the real buckets can be listed and read",
		DisableSliceFlagSeparator: true,
		Flags:                     s3FromFlags(),
		Action: func(ctx context.Context, cmd *cli.Command) error {
			out := cmd.Root().Writer

			lowers, err := s3Lowers(cmd)
			if err != nil {
				return err
			}

			for _, lower := range lowers {
				if err := s3Check(ctx, out, lower); err != nil {
					return err
				}
			}

			fmt.Fprintln(out, "blobfork can't tell whether the key could also write: give it one that can only read and list (s3:GetObject, s3:ListBucket).")

			return nil
		},
	}
}

// s3Check lists a real bucket and reads an object of it.
func s3Check(ctx context.Context, out io.Writer, lower *s3.Bucket) error {
	page, err := lower.List(ctx, s3.ListQuery{MaxKeys: 10})
	if err != nil {
		return fmt.Errorf("listing s3://%s: %w", lower.Name, err)
	}

	more := ""
	if page.Next != "" {
		more = ", more after"
	}

	fmt.Fprintf(out, "listed s3://%s at %s: %d objects on the first page%s\n", lower.Name, lower.Endpoint, len(page.Entries), more)

	if len(page.Entries) == 0 {
		return nil
	}

	key := page.Entries[0].Name

	res, err := lower.Object(ctx, http.MethodHead, key, nil, http.Header{})
	if err != nil {
		return fmt.Errorf("reading %s: %w", key, err)
	}

	res.Body.Close()

	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("reading %s: %s", key, res.Status)
	}

	fmt.Fprintf(out, "read s3://%s/%s: %s bytes\n", lower.Name, key, res.Header.Get("Content-Length"))

	return nil
}
