package cli

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/urfave/cli/v3"

	"github.com/stagdhq/blobfork/internal/s3"
)

// s3Command is the S3 front: a fork of a bucket, in front of Floci.
func s3Command(version string) *cli.Command {
	return &cli.Command{
		Name:  "s3",
		Usage: "fork an S3 bucket (AWS, R2, MinIO, …), in front of Floci",
		Commands: []*cli.Command{
			s3ServeCommand(version),
			s3CheckCommand(),
		},
	}
}

// s3FromFlags are the real bucket and how to reach it, for serve and check.
func s3FromFlags() []cli.Flag {
	return []cli.Flag{
		&cli.StringFlag{
			Name:     "from",
			Usage:    "the real bucket, s3://<bucket>",
			Sources:  cli.EnvVars("BLOBFORK_FROM"),
			Required: true,
		},
		&cli.StringFlag{
			Name:        "from-endpoint",
			Usage:       "the real bucket's S3 endpoint, for R2, MinIO and the like",
			Sources:     cli.EnvVars("BLOBFORK_FROM_ENDPOINT"),
			DefaultText: "AWS's, https://s3.<region>.amazonaws.com",
		},
		&cli.StringFlag{
			Name:    "from-region",
			Usage:   "the real bucket's region",
			Value:   "us-east-1",
			Sources: cli.EnvVars("BLOBFORK_FROM_REGION"),
		},
		&cli.BoolFlag{
			Name:        "from-path-style",
			Usage:       "address the real bucket in the path, not the host",
			Sources:     cli.EnvVars("BLOBFORK_FROM_PATH_STYLE"),
			DefaultText: "with --from-endpoint",
		},
		&cli.StringFlag{
			Name:     "from-access-key-id",
			Usage:    "an access key that can read and list the real bucket, and nothing more",
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

// s3Lower is the real bucket, from the flags.
func s3Lower(cmd *cli.Command) (*s3.Bucket, error) {
	from, err := url.Parse(cmd.String("from"))
	if err != nil || from.Scheme != "s3" || from.Host == "" || strings.Trim(from.Path, "/") != "" {
		return nil, fmt.Errorf("--from: not a bucket, s3://<bucket>: %s", cmd.String("from"))
	}

	region := cmd.String("from-region")

	endpoint := cmd.String("from-endpoint")
	pathStyle := cmd.Bool("from-path-style")

	if endpoint == "" {
		endpoint = "https://s3." + region + ".amazonaws.com"
	} else if !cmd.IsSet("from-path-style") {
		pathStyle = true
	}

	return &s3.Bucket{
		Endpoint:  endpoint,
		Name:      from.Host,
		Region:    region,
		PathStyle: pathStyle,
		Credentials: aws.Credentials{
			AccessKeyID:     cmd.String("from-access-key-id"),
			SecretAccessKey: cmd.String("from-secret-access-key"),
			SessionToken:    cmd.String("from-session-token"),
		},
	}, nil
}

func s3ServeCommand(version string) *cli.Command {
	return &cli.Command{
		Name:  "serve",
		Usage: "serve the fork: the app's S3 endpoint, in front of Floci",
		Description: "Requests pass through to Floci, which must check signatures\n" +
			"(FLOCI_AUTH_VALIDATE_SIGNATURES=true). Reads the fork can't answer come from the\n" +
			"real bucket, deletes of its objects leave a marker, listings merge both, and copies\n" +
			"from it copy the object up first. The real bucket is never written.",
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
			&cli.StringFlag{
				Name:        "bucket",
				Usage:       "the forked bucket's name, if not the real one's",
				Sources:     cli.EnvVars("BLOBFORK_BUCKET"),
				DefaultText: "the real one's",
			},
			logFlag(),
		),
		Action: func(ctx context.Context, cmd *cli.Command) error {
			log, err := logger(cmd)
			if err != nil {
				return err
			}

			lower, err := s3Lower(cmd)
			if err != nil {
				return err
			}

			name := cmd.String("bucket")
			if name == "" {
				name = lower.Name
			}

			fork, err := s3.New(ctx, s3.Config{
				Floci: cmd.String("floci"),
				Credentials: aws.Credentials{
					AccessKeyID:     cmd.String("access-key-id"),
					SecretAccessKey: cmd.String("secret-access-key"),
				},
				Region: cmd.String("region"),
				Bucket: name,
				Lower:  lower,
				Log:    log,
			})
			if err != nil {
				return err
			}

			log.Info("blobfork: serving", "version", version, "from", "s3://"+lower.Name, "endpoint", lower.Endpoint, "bucket", name)

			return serve(ctx, cmd.String("listen"), fork, log)
		},
	}
}

func s3CheckCommand() *cli.Command {
	return &cli.Command{
		Name:  "check",
		Usage: "check that the real bucket can be listed and read",
		Flags: s3FromFlags(),
		Action: func(ctx context.Context, cmd *cli.Command) error {
			out := cmd.Root().Writer

			lower, err := s3Lower(cmd)
			if err != nil {
				return err
			}

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

			fmt.Fprintf(out, "read %s: %s bytes\n", key, res.Header.Get("Content-Length"))
			fmt.Fprintln(out, "blobfork can't tell whether the key could also write: give it one that can only read and list (s3:GetObject, s3:ListBucket).")

			return nil
		},
	}
}
