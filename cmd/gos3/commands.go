package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/seanmmitchell/gos3/internal/config"
	"github.com/seanmmitchell/gos3/internal/discovery"
	"github.com/seanmmitchell/gos3/internal/iam"
	"github.com/seanmmitchell/gos3/internal/version"
)

func cmdVersion(args []string, stdout, stderr io.Writer) error {
	fs := newFlagSet("version", "gos3 version [--json]", stderr)
	asJSON := fs.Bool("json", false, "print JSON")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if *asJSON {
		return writeJSONTo(stdout, version.Get())
	}
	_, err := fmt.Fprintln(stdout, version.String())
	return err
}

func cmdConfig(args []string, stdout, stderr io.Writer) error {
	sub := ""
	if len(args) > 0 {
		sub, args = args[0], args[1:]
	}
	switch sub {
	case "print":
		fs := newFlagSet("config print", "gos3 config print [serve flags]", stderr)
		cf := addConfigFlags(fs)
		if err := parseFlags(fs, args); err != nil {
			return err
		}
		cfg, err := config.Load(cf.configPath, cf.overrides()...)
		if err != nil {
			return err
		}
		data, err := cfg.Redacted().YAML()
		if err != nil {
			return err
		}
		fmt.Fprintln(stdout, "# gos3 effective configuration (defaults < file < GOS3_* env < flags; secrets redacted)")
		if cfg.Auth.RootAccessKey == "" {
			fmt.Fprintln(stdout, "# root credentials are not configured: generated on first start and stored in <data>/.gos3/root-credentials.json")
		}
		if _, err := stdout.Write(data); err != nil {
			return err
		}
		if err := cfg.Validate(); err != nil {
			return fmt.Errorf("the configuration is invalid: %w", err)
		}
		return nil
	case "init":
		fs := newFlagSet("config init", "gos3 config init [-o FILE] [--force]", stderr)
		out := fs.String("o", "", "write to FILE instead of standard output")
		force := fs.Bool("force", false, "overwrite an existing file")
		if err := parseFlags(fs, args); err != nil {
			return err
		}
		if *out == "" {
			_, err := io.WriteString(stdout, sampleConfig)
			return err
		}
		flags := os.O_WRONLY | os.O_CREATE | os.O_EXCL
		if *force {
			flags = os.O_WRONLY | os.O_CREATE | os.O_TRUNC
		}
		f, err := os.OpenFile(*out, flags, 0o600)
		if err != nil {
			if errors.Is(err, os.ErrExist) {
				return fmt.Errorf("%s already exists (use --force to overwrite)", *out)
			}
			return err
		}
		if _, err := io.WriteString(f, sampleConfig); err != nil {
			_ = f.Close()
			return err
		}
		if err := f.Close(); err != nil {
			return err
		}
		fmt.Fprintf(stderr, "wrote %s\n", *out)
		return nil
	case "env":
		fs := newFlagSet("config env", "gos3 config env", stderr)
		if err := parseFlags(fs, args); err != nil {
			return err
		}
		return config.WriteEnvTable(stdout)
	case "-h", "--help", "help":
		fmt.Fprintln(stdout, "Usage: gos3 config print|init|env [flags]")
		return nil
	default:
		fmt.Fprintln(stderr, "Usage: gos3 config print|init|env [flags]")
		return errUsage
	}
}

func cmdDiscover(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := newFlagSet("discover", "gos3 discover [flags]", stderr)
	wait := fs.Duration("wait", 3*time.Second, "how long to listen for mDNS and beacon answers")
	asJSON := fs.Bool("json", false, "print the peers as JSON")
	var seeds, srv listFlag
	fs.Var(&seeds, "seed", "also probe this base URL (repeatable)")
	fs.Var(&srv, "dns-srv", "also resolve _gos3._tcp.<domain> SRV records (repeatable)")
	noMDNS := fs.Bool("no-mdns", false, "do not query mDNS")
	noBeacon := fs.Bool("no-beacon", false, "do not query the UDP beacon")
	group := fs.String("beacon-group", discovery.DefaultBeaconGroup, "beacon multicast group ip:port")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	cfg := discovery.DefaultConfig()
	cfg.MDNS, cfg.Beacon, cfg.BeaconGroup = !*noMDNS, !*noBeacon, *group
	cfg.Seeds, cfg.DNSSRV = seeds, srv
	peers, err := discovery.Browse(ctx, cfg, *wait)
	if err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	if peers == nil {
		peers = []discovery.Peer{}
	}
	if *asJSON {
		return writeJSONTo(stdout, peers)
	}
	if len(peers) == 0 {
		fmt.Fprintln(stdout, "no gos3 servers found")
		return nil
	}
	tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tID\tVERSION\tREGION\tS3 ENDPOINT\tSOURCES")
	for _, p := range peers {
		src := make([]string, len(p.Sources))
		for i, s := range p.Sources {
			src[i] = string(s)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", orDash(p.Name), p.ID, orDash(p.Version), orDash(p.Region),
			orDash(p.Endpoints.S3), strings.Join(src, ","))
	}
	return tw.Flush()
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func cmdKeygen(args []string, stdout, stderr io.Writer) error {
	fs := newFlagSet("keygen", "gos3 keygen [--json]", stderr)
	asJSON := fs.Bool("json", false, "print JSON")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	ak, err := iam.GenerateAccessKey()
	if err != nil {
		return err
	}
	sk, err := iam.GenerateSecretKey()
	if err != nil {
		return err
	}
	if *asJSON {
		return writeJSONTo(stdout, map[string]string{"accessKey": ak, "secretKey": sk})
	}
	_, err = fmt.Fprintf(stdout, "GOS3_ROOT_ACCESS_KEY=%s\nGOS3_ROOT_SECRET_KEY=%s\n", ak, sk)
	return err
}

func cmdHealthcheck(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := newFlagSet("healthcheck", "gos3 healthcheck [--url URL] [--ready]", stderr)
	rawURL := fs.String("url", "", "server base URL (default: derived from the configured listen address)")
	configPath := fs.String("config", "", "YAML configuration file used to derive the URL (env GOS3_CONFIG)")
	ready := fs.Bool("ready", false, "check /_gos3/ready (backend reachable) instead of /_gos3/health")
	timeout := fs.Duration("timeout", 5*time.Second, "request timeout")
	insecure := fs.Bool("insecure", false, "skip TLS certificate verification (default: on for derived loopback URLs)")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	base := strings.TrimSuffix(*rawURL, "/")
	skipVerify := *insecure
	if base == "" {
		cfg, err := config.Load(*configPath)
		if err != nil {
			return err
		}
		host, port, err := net.SplitHostPort(cfg.Server.Address)
		if err != nil {
			return fmt.Errorf("server.address %q: %w", cfg.Server.Address, err)
		}
		if ip := net.ParseIP(host); host == "" || (ip != nil && ip.IsUnspecified()) {
			host = "127.0.0.1"
		}
		scheme := "http"
		if cfg.TLSEnabled() {
			// We are checking our own server, typically with a certificate
			// that is not valid for the loopback address.
			scheme, skipVerify = "https", true
		}
		base = scheme + "://" + net.JoinHostPort(host, port)
	}
	path := "/_gos3/health"
	if *ready {
		path = "/_gos3/ready"
	}
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+path, nil)
	if err != nil {
		return err
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = nil
	if skipVerify {
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // explicit opt-in / own loopback server
	}
	resp, err := (&http.Client{Transport: tr}).Do(req)
	if err != nil {
		return fmt.Errorf("unhealthy: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	var st struct {
		Status string `json:"status"`
		Error  string `json:"error"`
	}
	_ = json.Unmarshal(body, &st)
	if resp.StatusCode != http.StatusOK || st.Status != "ok" {
		msg := strings.TrimSpace(st.Error)
		if msg == "" {
			msg = strings.TrimSpace(string(body))
		}
		return fmt.Errorf("unhealthy: %s %s: HTTP %d %s", req.Method, req.URL, resp.StatusCode, msg)
	}
	_, err = fmt.Fprintln(stdout, "ok")
	return err
}

func writeJSONTo(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
