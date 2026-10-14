// Command gos3 is an S3-compatible object storage server with an embedded
// web UI and network discovery.
//
// Usage:
//
//	gos3 [serve|server] [flags] [data] run the server (default command)
//	gos3 version                print the version
//	gos3 config print [flags]   print the effective configuration (secrets redacted)
//	gos3 config init [-o file]  write a commented sample configuration
//	gos3 config env             list the GOS3_* environment variables
//	gos3 discover [flags]       find gos3 servers on the network
//	gos3 keygen                 generate a random access/secret key pair
//	gos3 healthcheck [--url]    probe a server's health endpoint (exit code)
//
// Configuration is read from defaults < JSON/YAML file (--config / GOS3_CONFIG)
// < GOS3_* environment variables < flags.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	go func() {
		// After the first signal restore the default behaviour, so a second
		// Ctrl-C terminates immediately instead of waiting for shutdown.
		<-ctx.Done()
		stop()
	}()
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

// errUsage marks command line errors (exit code 2); the message has been
// printed already.
var errUsage = errors.New("usage error")

// run executes the command line args and returns the process exit code.
func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	cmd, rest := "serve", args
	if len(args) > 0 && (len(args[0]) == 0 || args[0][0] != '-') {
		cmd, rest = args[0], args[1:]
	}
	var err error
	switch cmd {
	case "serve", "server":
		err = cmdServe(ctx, rest, stdout, stderr)
	case "version":
		err = cmdVersion(rest, stdout, stderr)
	case "config":
		err = cmdConfig(rest, stdout, stderr)
	case "discover":
		err = cmdDiscover(ctx, rest, stdout, stderr)
	case "keygen":
		err = cmdKeygen(rest, stdout, stderr)
	case "keys": // "gos3 keys gen", as in docs/ARCHITECTURE.md
		if len(rest) == 0 || rest[0] != "gen" {
			fmt.Fprintln(stderr, "usage: gos3 keys gen [--json]")
			return 2
		}
		err = cmdKeygen(rest[1:], stdout, stderr)
	case "healthcheck":
		err = cmdHealthcheck(ctx, rest, stdout, stderr)
	case "help", "-h", "--help":
		usage(stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "gos3: unknown command %q\n\n", cmd)
		usage(stderr)
		return 2
	}
	switch {
	case err == nil:
		return 0
	case errors.Is(err, flag.ErrHelp):
		return 0
	case errors.Is(err, errUsage):
		return 2
	default:
		fmt.Fprintf(stderr, "gos3: %v\n", err)
		return 1
	}
}

func usage(w io.Writer) {
	fmt.Fprint(w, `gos3 — S3-compatible object storage server

Usage:
  gos3 [serve|server] [flags] [data] run the server (default; see "gos3 serve -h")
  gos3 version                  print the version
  gos3 config print [flags]     print the effective configuration (secrets redacted)
  gos3 config init [--json] [-o FILE] write a sample configuration file
  gos3 config env               list the GOS3_* environment variables
  gos3 discover [flags]         find gos3 servers on the local network
  gos3 keygen [--json]          generate a random access/secret key pair
  gos3 healthcheck [--url URL]  check a server's health (exit 0 = healthy)

Configuration: defaults < JSON/YAML file (--config or GOS3_CONFIG) < GOS3_*
environment variables < flags. Root credentials are set only via the
environment (GOS3_ROOT_ACCESS_KEY / GOS3_ROOT_SECRET_KEY) or the config file;
without them gos3 generates a pair on first start and stores it in
<data>/.gos3/root-credentials.json.
`)
}

// newFlagSet returns a FlagSet that reports errors through run (not by
// exiting) and prints its usage to stderr.
func newFlagSet(name, synopsis string, stderr io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprintf(stderr, "Usage: %s\n\nFlags:\n", synopsis)
		fs.PrintDefaults()
	}
	return fs
}

// parseFlags parses args and rejects positional arguments.
func parseFlags(fs *flag.FlagSet, args []string) error {
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return err
		}
		return errUsage
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(fs.Output(), "unexpected argument %q\n", fs.Arg(0))
		fs.Usage()
		return errUsage
	}
	return nil
}
