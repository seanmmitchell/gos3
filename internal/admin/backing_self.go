package admin

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/seanmmitchell/gos3/internal/discovery"
	"github.com/seanmmitchell/gos3/internal/storage"
)

// selfProbeTimeout bounds checkNotSelf's identity request.
const selfProbeTimeout = 5 * time.Second

// errBackingSelf refuses S3 storage on this server itself.
var errBackingSelf = badRequest("the S3 endpoint cannot be this gos3 server: its data would be stored back into this server. Use another S3 service.")

// checkNotSelf refuses an S3 endpoint that is this server, for storage
// gos3 itself writes through S3 (such as a backing on a remote S3
// service): its writes would land in buckets of the same server, possibly
// recursing. The endpoint is compared with the base URLs this server
// advertises and, as those may not cover proxies and DNS names, its
// GET /_gos3/info identity document is fetched and the server ID compared
// (best effort: an endpoint without a readable gos3 identity passes).
func (a *api) checkNotSelf(ctx context.Context, endpoint string) error {
	self := a.info()
	if self.ID == "" {
		return nil
	}
	want := originOf(endpoint)
	for _, u := range append([]string{self.Endpoints.S3}, self.Endpoints.All...) {
		if want != "" && originOf(u) == want {
			return errBackingSelf
		}
	}
	ctx, cancel := context.WithTimeout(ctx, selfProbeTimeout)
	defer cancel()
	info, err := fetchIdentity(ctx, endpoint)
	if err != nil {
		a.log.Debug("admin: S3 endpoint has no gos3 identity", "endpoint", endpoint, "err", err)
		return nil
	}
	if info.ID == self.ID {
		return errBackingSelf
	}
	return nil
}

// originOf returns "scheme://host:port" of an http(s) URL in lower case
// with the default port made explicit, or "" when raw is not one.
func originOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		return ""
	}
	port := u.Port()
	if port == "" {
		port = map[string]string{"http": "80", "https": "443"}[u.Scheme]
	}
	host := strings.ToLower(u.Hostname())
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	return u.Scheme + "://" + host + ":" + port
}

// fetchIdentity performs GET <base>/_gos3/info without following
// redirects and returns the validated identity document.
func fetchIdentity(ctx context.Context, base string) (discovery.Info, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(base, "/")+discovery.InfoPath, nil)
	if err != nil {
		return discovery.Info{}, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", storage.ResticUserAgent+"-check")
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return discovery.Info{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return discovery.Info{}, errors.New(resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, discovery.MaxInfoSize+1))
	if err != nil {
		return discovery.Info{}, err
	}
	return discovery.ParseInfo(body)
}
