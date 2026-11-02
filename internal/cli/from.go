package cli

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/urfave/cli/v3"
)

// fromFlag is the real containers or buckets to fork, any number: the flag
// again for each, or several in BLOBFORK_FROM, apart by spaces or lines.
// The commands that take it turn off the list's comma separator, as a SAS
// may have commas in it.
func fromFlag(usage string) *cli.StringSliceFlag {
	return &cli.StringSliceFlag{
		Name:     "from",
		Usage:    usage + "; again for each, and <name>=… forks it under another name",
		Sources:  cli.EnvVars("BLOBFORK_FROM"),
		Required: true,
	}
}

// from is one --from: the real container or bucket, and the name of its fork
// if not its own.
type from struct {
	name string
	raw  string
}

// forkName is what a container or bucket may be called: lowercase letters,
// digits, dots and hyphens, as both Azure and S3 allow.
var forkName = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,62}$`)

func froms(cmd *cli.Command) ([]from, error) {
	var out []from

	for _, value := range cmd.StringSlice("from") {
		for _, raw := range strings.Fields(value) {
			f := from{raw: raw}

			// A URL's first "=" is in its query, after its "://".
			if name, rest, ok := strings.Cut(raw, "="); ok && !strings.Contains(name, ":") {
				if !forkName.MatchString(name) {
					return nil, fmt.Errorf("--from: not a container or bucket name: %q", name)
				}

				f = from{name: name, raw: rest}
			}

			out = append(out, f)
		}
	}

	if len(out) == 0 {
		return nil, fmt.Errorf("--from: nothing to fork")
	}

	return out, nil
}
