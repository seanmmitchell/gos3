package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/seanmmitchell/gos3/internal/config"
	"github.com/seanmmitchell/gos3/internal/server"
	"github.com/seanmmitchell/gos3/internal/version"
)

// configFlags are the flags that override configuration keys (shared by
// serve and config print).
type configFlags struct {
	fs         *flag.FlagSet
	configPath string
	keys       map[string]string                // flag name -> config key
	values     map[string]func() (string, bool) // flag name -> override value (ok=false: skip)
}

// listFlag is a repeatable string flag; each value may also be a
// comma-separated list.
type listFlag []string

func (l *listFlag) String() string { return strings.Join(*l, ",") }
func (l *listFlag) Set(v string) error {
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			*l = append(*l, p)
		}
	}
	return nil
}

// addConfigFlags registers the configuration flags on fs.
func addConfigFlags(fs *flag.FlagSet) *configFlags {
	c := &configFlags{fs: fs, keys: map[string]string{}, values: map[string]func() (string, bool){}}
	fs.StringVar(&c.configPath, "config", "", "YAML configuration file (env GOS3_CONFIG)")

	str := func(name, key, usage string) {
		p := fs.String(name, "", usage)
		c.keys[name] = key
		c.values[name] = func() (string, bool) { return *p, true }
	}
	list := func(name, key, usage string) {
		var l listFlag
		fs.Var(&l, name, usage)
		c.keys[name] = key
		c.values[name] = func() (string, bool) { return strings.Join(l, ","), true }
	}
	boolean := func(name, key string, invert bool, usage string) {
		p := fs.Bool(name, false, usage)
		c.keys[name] = key
		c.values[name] = func() (string, bool) { return strconv.FormatBool(*p != invert), true }
	}

	str("address", "server.address", "listen address [host]:port (default \":9000\")")
	str("data", "storage.path", "data directory (default \"./data\"; memory backend: none = ephemeral)")
	str("backend", "storage.backend", "storage backend: fs or memory (default fs)")
	str("name", "server.name", "server name shown in the UI and discovery (default: host name)")
	str("region", "server.region", "region reported to clients (default us-east-1)")
	str("public-url", "server.public_url", "externally reachable base URL, e.g. https://s3.example.com")
	list("domain", "server.domains", "virtual-host domain (repeatable): <bucket>.<domain> addresses a bucket")
	str("tls-cert", "server.tls_cert", "TLS certificate file (PEM); enables HTTPS together with --tls-key")
	str("tls-key", "server.tls_key", "TLS private key file (PEM)")
	list("cors-origin", "cors.allowed_origins", "allowed CORS origin (repeatable; default \"*\")")
	list("seed", "discovery.seeds", "discovery seed base URL (repeatable), e.g. http://nas:9000")
	boolean("no-ui", "ui.enabled", true, "disable the embedded web UI")
	boolean("no-discovery", "discovery.enabled", true, "disable discovery (mDNS, beacon, seeds, DNS SRV)")
	str("log-level", "logging.level", "log level: debug, info, warn or error (default info)")
	str("log-format", "logging.format", "log format: text or json (default text)")
	boolean("fsync", "storage.fsync_data", false, "fsync object data on commit (default true; --fsync=false is faster but less durable)")
	return c
}

// overrides returns the config overrides of the flags that were set.
func (c *configFlags) overrides() []config.Override {
	var out []config.Override
	c.fs.Visit(func(f *flag.Flag) {
		key, ok := c.keys[f.Name]
		if !ok {
			return
		}
		if v, ok := c.values[f.Name](); ok {
			out = append(out, config.Override{Key: key, Value: v})
		}
	})
	return out
}

// load loads and validates the configuration with the flag overrides.
func (c *configFlags) load() (config.Config, error) {
	cfg, err := config.Load(c.configPath, c.overrides()...)
	if err != nil {
		return config.Config{}, err
	}
	return cfg, cfg.Validate()
}

// onServerReady is a test hook called once the server listens.
var onServerReady func(*server.Server)

func cmdServe(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := newFlagSet("serve", "gos3 serve [flags]", stderr)
	cf := addConfigFlags(fs)
	fs.Usage = func() {
		fmt.Fprint(stderr, "Usage: gos3 [serve] [flags]\n\nRuns the S3 server. Flags override the config file and GOS3_* variables\n(see \"gos3 config env\"); root credentials are only read from the\nenvironment or the config file.\n\nFlags:\n")
		fs.PrintDefaults()
	}
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	cfg, err := cf.load()
	if err != nil {
		return err
	}

	logger, ctl := server.NewLogger(stderr, cfg.Logging.Level, cfg.Logging.Format)
	slog.SetDefault(logger)
	logger.Info("starting " + version.String())

	srv, err := server.New(cfg, logger, server.WithLogControl(ctl))
	if err != nil {
		return err
	}
	go func() {
		select {
		case <-srv.Ready():
		case <-ctx.Done():
			return
		}
		if ctl.JSON() {
			info := srv.Info()
			logger.Info("gos3 ready", "url", srv.URL(), "endpoints", info.Endpoints.All, "ui", info.Endpoints.UI,
				"backend", srv.BackendName(), "data", srv.DataPath(), "credentials", srv.CredentialsSource())
		} else {
			printBanner(stderr, srv)
		}
		if onServerReady != nil {
			onServerReady(srv)
		}
	}()
	return srv.Run(ctx)
}

// printBanner prints the human-friendly startup summary.
func printBanner(w io.Writer, srv *server.Server) {
	info := srv.Info()
	local := srv.URL()
	ak, sk := srv.RootCredentials()

	var b strings.Builder
	tw := tabwriter.NewWriter(&b, 0, 4, 2, ' ', 0)
	fmt.Fprintf(&b, "\n  %s — S3-compatible object storage\n\n", version.String())
	urls := []string{local}
	for _, u := range info.Endpoints.All {
		if u != local {
			urls = append(urls, u)
		}
	}
	fmt.Fprintf(tw, "  S3 endpoint\t%s\n", urls[0])
	for _, u := range urls[1:] {
		fmt.Fprintf(tw, "  \t%s\n", u)
	}
	if srv.UIEnabled() {
		ui := local + "/_gos3/ui/"
		if !srv.UIBuilt() {
			ui += "  (placeholder: UI not built, run make build)"
		}
		fmt.Fprintf(tw, "  Web UI\t%s\n", ui)
	} else {
		fmt.Fprintf(tw, "  Web UI\tdisabled\n")
	}
	fmt.Fprintf(tw, "  Admin API\t%s/_gos3/admin/v1\n", local)
	fmt.Fprintf(tw, "  Region\t%s\n", info.Region)
	switch {
	case srv.BackendName() == config.BackendFS:
		fmt.Fprintf(tw, "  Storage\tfs at %s\n", srv.DataPath())
	case srv.DataPath() != "":
		fmt.Fprintf(tw, "  Storage\tmemory (ephemeral objects; system state in %s)\n", srv.DataPath())
	default:
		fmt.Fprintf(tw, "  Storage\tmemory (ephemeral: everything is lost on exit)\n")
	}
	fmt.Fprintf(tw, "  Access key\t%s\n", ak)
	switch srv.CredentialsSource() {
	case config.CredentialsConfigured:
		fmt.Fprintf(tw, "  Secret key\t(from the configuration / environment)\n")
	case config.CredentialsStored:
		fmt.Fprintf(tw, "  Secret key\t(generated earlier, stored in %s)\n", srv.RootCredentialsFile())
	case config.CredentialsGenerated:
		fmt.Fprintf(tw, "  Secret key\t%s\n", sk)
		fmt.Fprintf(tw, "  \t(generated now, stored in %s; set GOS3_ROOT_ACCESS_KEY/GOS3_ROOT_SECRET_KEY to choose your own)\n", srv.RootCredentialsFile())
	case config.CredentialsEphemeral:
		fmt.Fprintf(tw, "  Secret key\t%s\n", sk)
		fmt.Fprintf(tw, "  \t(temporary: valid until this process exits)\n")
	}
	_ = tw.Flush()
	b.WriteString("\n  Press Ctrl-C to stop.\n\n")
	_, _ = io.WriteString(w, b.String())
}
