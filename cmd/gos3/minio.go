package main

import (
	"errors"
	"flag"
	"fmt"
	"path/filepath"
	"strings"
)

// parseServerFlags accepts a single local data directory before or after
// flags, as in "minio server /data --address :9000". It deliberately rejects
// distributed MinIO volume syntax instead of treating it as a local path.
func parseServerFlags(fs *flag.FlagSet, args []string) error {
	fail := func(message string) error {
		fmt.Fprintln(fs.Output(), message)
		return errUsage
	}
	var paths []string
	for {
		if err := fs.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return err
			}
			return errUsage
		}
		if fs.NArg() == 0 {
			break
		}
		paths = append(paths, fs.Arg(0))
		args = fs.Args()[1:]
	}
	if len(paths) > 1 {
		return fail("only one local data directory is supported; configure storage.backings for additional disks")
	}
	if len(paths) == 1 {
		p := paths[0]
		if p == "" || strings.Contains(p, "://") || strings.ContainsAny(p, "{}") {
			return fail("data directory must be a local path; MinIO distributed volume syntax is not supported")
		}
		hasData := false
		fs.Visit(func(f *flag.Flag) { hasData = hasData || f.Name == "data" })
		if hasData {
			return fail("use either --data or a positional data directory, not both")
		}
		return fs.Set("data", filepath.Clean(p))
	}
	return nil
}
