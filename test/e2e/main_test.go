package e2e

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/seanmmitchell/gos3/internal/auth"
	"github.com/seanmmitchell/gos3/internal/config"
	"github.com/seanmmitchell/gos3/internal/server"
)

const (
	region     = "us-east-1"
	vhostZone  = "s3.test"
	rootAccess = "e2eroot"
	rootSecret = "e2eroot-secret-0123456789"
)

var rootCreds = auth.Credentials{AccessKey: rootAccess, SecretKey: rootSecret}

// Shared servers started by TestMain.
var (
	fsEnv  *env // fs backend, plain HTTP, virtual-host domain s3.test
	memEnv *env // memory backend, HTTPS
)

// env is one running gos3 server plus what clients need to reach it.
type env struct {
	name  string
	srv   *server.Server
	cfg   config.Config
	base  string // http(s)://127.0.0.1:port
	addr  string // 127.0.0.1:port
	tls   bool
	httpc *http.Client // dials addr for every host name; trusts the test CA

	cancel context.CancelFunc
	errc   chan error
	logs   *logBuffer
}

func TestMain(m *testing.M) {
	os.Exit(runMain(m))
}

func runMain(m *testing.M) int {
	tmp, err := os.MkdirTemp("", "gos3-e2e-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "e2e:", err)
		return 1
	}
	defer os.RemoveAll(tmp)

	certFile, keyFile, caPool, err := selfSignedCert(tmp)
	if err != nil {
		fmt.Fprintln(os.Stderr, "e2e: certificate:", err)
		return 1
	}
	testCA = caPool

	fsCfg := baseConfig()
	fsCfg.Storage.Backend = config.BackendFS
	fsCfg.Storage.Path = filepath.Join(tmp, "fs-data")
	fsCfg.Storage.FsyncData = false
	fsCfg.Server.Domains = []string{vhostZone}
	if fsEnv, err = startEnv("fs", fsCfg); err != nil {
		fmt.Fprintln(os.Stderr, "e2e: starting fs server:", err)
		return 1
	}
	defer fsEnv.stop()

	memCfg := baseConfig()
	memCfg.Storage.Backend = config.BackendMemory
	memCfg.Storage.Path = ""
	memCfg.Server.TLSCert = certFile
	memCfg.Server.TLSKey = keyFile
	if memEnv, err = startEnv("mem", memCfg); err != nil {
		fmt.Fprintln(os.Stderr, "e2e: starting memory server:", err)
		return 1
	}
	defer memEnv.stop()

	code := m.Run()
	if code != 0 && dumpLogs {
		fmt.Fprintf(os.Stderr, "---- fs server log ----\n%s\n---- mem server log ----\n%s\n", fsEnv.logs, memEnv.logs)
	}
	return code
}

// testCA trusts the self-signed certificate of the TLS servers.
var testCA *x509.CertPool

// baseConfig is the common configuration of every e2e server: loopback,
// fixed root credentials, region us-east-1, no discovery (no multicast in
// tests), no scheduled lifecycle passes (tests run them explicitly).
func baseConfig() config.Config {
	cfg := config.Default()
	cfg.Server.Address = "127.0.0.1:0"
	cfg.Server.Name = "gos3-e2e"
	cfg.Server.Region = region
	cfg.Server.ShutdownTimeout = 10 * time.Second
	cfg.Auth.RootAccessKey = rootAccess
	cfg.Auth.RootSecretKey = rootSecret
	cfg.Discovery.Enabled = false
	cfg.Lifecycle.Enabled = false
	cfg.Logging.AccessLog = true
	cfg.Logging.Level = "debug"
	cfg.UI.Enabled = false
	return cfg
}

// startEnv creates and serves a server on a random loopback port.
func startEnv(name string, cfg config.Config) (*env, error) {
	logs := &logBuffer{}
	logger := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	srv, err := server.New(cfg, logger)
	if err != nil {
		return nil, err
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		_ = srv.Close()
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	e := &env{name: name, srv: srv, cfg: cfg, cancel: cancel, errc: make(chan error, 1), logs: logs}
	go func() { e.errc <- srv.Serve(ctx, ln) }()
	select {
	case <-srv.Ready():
	case err := <-e.errc:
		cancel()
		return nil, fmt.Errorf("serve: %w", err)
	case <-time.After(15 * time.Second):
		cancel()
		return nil, errors.New("server did not become ready")
	}
	e.addr = srv.Addr()
	e.base = srv.URL()
	e.tls = strings.HasPrefix(e.base, "https://")
	e.httpc = newHTTPClient(e.addr)
	return e, nil
}

// stop shuts the server down gracefully and returns Serve's error.
func (e *env) stop() error {
	if e.cancel == nil {
		return nil
	}
	e.cancel()
	e.cancel = nil
	select {
	case err := <-e.errc:
		e.httpc.CloseIdleConnections()
		return err
	case <-time.After(30 * time.Second):
		return errors.New("server did not stop")
	}
}

// newHTTPClient returns a client whose connections all go to addr, whatever
// the URL's host (virtual-host names like bucket.s3.test resolve nowhere),
// trusting the test CA. Proxies from the environment are never used.
func newHTTPClient(addr string) *http.Client {
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	tr := &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, network, addr)
		},
		TLSClientConfig:     &tls.Config{RootCAs: testCA, ServerName: "localhost"},
		MaxIdleConns:        256,
		MaxIdleConnsPerHost: 64,
		IdleConnTimeout:     30 * time.Second,
		ForceAttemptHTTP2:   false,
	}
	return &http.Client{
		Transport: tr,
		Timeout:   5 * time.Minute,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// selfSignedCert writes a self-signed ECDSA certificate for localhost and
// 127.0.0.1 to dir and returns the file names and a pool trusting it.
func selfSignedCert(dir string) (certFile, keyFile string, pool *x509.CertPool, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", "", nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{CommonName: "gos3 e2e"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		DNSNames:              []string{"localhost", "*.s3.test", "s3.test"},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return "", "", nil, err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return "", "", nil, err
	}
	certFile = filepath.Join(dir, "cert.pem")
	keyFile = filepath.Join(dir, "key.pem")
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		return "", "", nil, err
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		return "", "", nil, err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return "", "", nil, err
	}
	pool = x509.NewCertPool()
	pool.AddCert(cert)
	return certFile, keyFile, pool, nil
}

// logBuffer collects server logs (bounded) for E2E_DUMP_LOGS; with
// E2E_VERBOSE they are also copied to stderr as they are written.
type logBuffer struct {
	mu  sync.Mutex
	buf []byte
}

const maxLogBytes = 8 << 20

var (
	dumpLogs    = os.Getenv("E2E_DUMP_LOGS") != ""
	verboseLogs = os.Getenv("E2E_VERBOSE") != ""
)

func (b *logBuffer) Write(p []byte) (int, error) {
	if verboseLogs {
		_, _ = os.Stderr.Write(p)
	}
	if !dumpLogs {
		return len(p), nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = append(b.buf, p...)
	if len(b.buf) > maxLogBytes {
		b.buf = append([]byte(nil), b.buf[len(b.buf)-maxLogBytes:]...)
	}
	return len(p), nil
}

func (b *logBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.buf)
}
