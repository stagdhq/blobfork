package cli

import (
	"context"
	"slices"
	"testing"

	"github.com/urfave/cli/v3"
)

// --from takes any number, by the flag again or apart by spaces and lines in
// one value, with <name>= to rename one; a SAS's commas stay in it.
func TestFroms(t *testing.T) {
	parse := func(args ...string) ([]from, error) {
		var got []from
		var err error

		cmd := &cli.Command{
			Name:                      "serve",
			DisableSliceFlagSeparator: true,
			Flags:                     []cli.Flag{fromFlag("a real one")},
			Action: func(_ context.Context, cmd *cli.Command) error {
				got, err = froms(cmd)

				return nil
			},
		}

		if runErr := cmd.Run(context.Background(), append([]string{"serve"}, args...)); runErr != nil {
			return nil, runErr
		}

		return got, err
	}

	sas := "https://acme.blob.core.windows.net/files?sv=2025-01-05&spr=https,http&sp=rl&sig=a%2Bb="

	got, err := parse("--from", sas, "--from", "images=https://acme.blob.core.windows.net/img?sig=x", "--from", "s3://a\n  s3://b")
	if err != nil {
		t.Fatal(err)
	}

	want := []from{
		{raw: sas},
		{name: "images", raw: "https://acme.blob.core.windows.net/img?sig=x"},
		{raw: "s3://a"},
		{raw: "s3://b"},
	}

	if !slices.Equal(got, want) {
		t.Fatalf("got  %v\nwant %v", got, want)
	}

	if _, err := parse("--from", "Files=s3://a"); err == nil {
		t.Fatal("a name with capitals")
	}

	if _, err := parse("--from", " "); err == nil {
		t.Fatal("nothing to fork")
	}
}
