// Package updates checks public GitHub releases on behalf of UI users.
// It never downloads assets, accepts credentials or blocks storage requests.
package updates

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"sync"
	"time"

	"github.com/seanmmitchell/gos3/internal/config"
)

const interval = 24 * time.Hour
const checkTimeout = 10 * time.Second
const releasesURL = "https://api.github.com/repos/seanmmitchell/gos3/releases"

// Status is a read-only snapshot. Error never replaces the last successful
// result; lastCheck identifies an attempt, lastSuccess a valid response.
type Status struct {
	Supported        bool       `json:"supported"`
	Enabled          bool       `json:"enabled"`
	InstalledVersion string     `json:"installedVersion"`
	Comparable       bool       `json:"comparable"`
	NotifyFor        []string   `json:"notifyFor"`
	LastCheck        *time.Time `json:"lastCheck"`
	LastSuccess      *time.Time `json:"lastSuccess"`
	Error            string     `json:"error"`
	Available        *Release   `json:"available"`
}

type cache struct {
	LastCheck   *time.Time `json:"lastCheck"`
	LastSuccess *time.Time `json:"lastSuccess"`
	Error       string     `json:"error"`
	Tags        []string   `json:"tags"`
}

// Service serializes all checks (including settings changes).
type Service struct {
	mu              sync.Mutex
	installed, path string
	settings        config.UpdatesConfig
	cache           cache
	wake            chan struct{}
	cancel          context.CancelFunc
	client          *http.Client
	endpoint        string
	now             func() time.Time
}

// New restores the successful result and daily attempt budget. A bad cache
// is informational: it must never prevent the server from starting.
func New(installed, path string) *Service {
	s := &Service{installed: installed, path: path, wake: make(chan struct{}, 1),
		client: &http.Client{Timeout: checkTimeout}, endpoint: releasesURL, now: time.Now}
	if path != "" {
		f, err := os.Open(path)
		if err == nil {
			defer f.Close()
			if err = json.NewDecoder(io.LimitReader(f, 4<<20)).Decode(&s.cache); err != nil {
				s.cache = cache{Error: "Could not read the saved update check; waiting for a new check"}
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			s.cache.Error = "Could not read the saved update check"
		}
	}
	return s
}

// Configure is synchronous and cancels outbound work when checks are disabled.
// Notification changes immediately select from the cached release list.
func (s *Service) Configure(c config.UpdatesConfig) {
	s.mu.Lock()
	s.settings = c
	s.settings.NotifyFor = slices.Clone(c.NotifyFor)
	if !c.Enabled && s.cancel != nil {
		s.cancel()
	}
	s.mu.Unlock()
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *Service) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := Status{Supported: true, Enabled: s.settings.Enabled, InstalledVersion: s.installed,
		Comparable: components(s.installed) != nil, NotifyFor: append([]string{}, s.settings.NotifyFor...),
		LastCheck: copyTime(s.cache.LastCheck), LastSuccess: copyTime(s.cache.LastSuccess), Error: s.cache.Error}
	if out.Enabled {
		out.Available = newest(s.installed, s.cache.Tags, s.settings.NotifyFor)
	}
	return out
}

func copyTime(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	value := *t
	return &value
}

// Run is called once with the server lifetime context. API reads never check
// GitHub; enabling checks wakes this loop immediately if a check is due.
func (s *Service) Run(ctx context.Context) {
	for {
		s.check(ctx)
		delay := interval
		s.mu.Lock()
		if s.settings.Enabled && components(s.installed) != nil && s.cache.LastCheck != nil {
			delay = max(time.Second, s.cache.LastCheck.Add(interval).Sub(s.now()))
		}
		s.mu.Unlock()
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-s.wake:
			timer.Stop()
		case <-timer.C:
		}
	}
}

func (s *Service) persist() {
	if s.path == "" {
		return
	}
	data, err := json.Marshal(s.cache)
	if err == nil {
		err = config.WriteFileAtomic(s.path, data, 0o600)
	}
	if err != nil {
		s.cache.Error = "Could not save the update check cache"
	}
}

func (s *Service) check(parent context.Context) {
	s.mu.Lock()
	now := s.now().UTC()
	if parent.Err() != nil || !s.settings.Enabled || components(s.installed) == nil ||
		(s.cache.LastCheck != nil && now.Sub(*s.cache.LastCheck) < interval) {
		s.mu.Unlock()
		return
	}
	ctx, cancel := context.WithTimeout(parent, checkTimeout)
	s.cancel = cancel
	s.cache.LastCheck = &now
	// Reserve the attempt before I/O, including across restarts and failures.
	s.persist()
	s.mu.Unlock()
	tags, err := s.fetch(ctx)
	cancel()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cancel = nil
	if err != nil {
		s.cache.Error = err.Error()
	} else {
		s.cache.Tags = tags
		at := s.now().UTC()
		s.cache.LastSuccess = &at
		s.cache.Error = ""
	}
	s.persist()
}

func (s *Service) fetch(ctx context.Context) ([]string, error) {
	tags := []string{}
	// Follow numbered pages of the fixed repository, never a remote Link URL.
	// Bound total work as well as each response and the entire check duration.
	for page := 1; page <= 20; page++ {
		req, err := http.NewRequestWithContext(ctx, "GET", fmt.Sprintf("%s?per_page=100&page=%d", s.endpoint, page), nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("User-Agent", "gos3-release-check")
		resp, err := s.client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("GitHub release check failed: %w", err)
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			return nil, fmt.Errorf("GitHub release check returned HTTP %d (next attempt in 24 hours)", resp.StatusCode)
		}
		var releases []struct {
			Tag        string     `json:"tag_name"`
			Draft      bool       `json:"draft"`
			Prerelease bool       `json:"prerelease"`
			Published  *time.Time `json:"published_at"`
		}
		data, err := io.ReadAll(io.LimitReader(resp.Body, (4<<20)+1))
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("reading GitHub releases: %w", err)
		}
		if len(data) > 4<<20 || json.Unmarshal(data, &releases) != nil || releases == nil {
			return nil, errors.New("GitHub returned an invalid release list")
		}
		for _, r := range releases {
			if r.Tag == "" {
				return nil, errors.New("GitHub returned an invalid release entry")
			}
			if !r.Draft && !r.Prerelease && r.Published != nil && components(r.Tag) != nil {
				tags = append(tags, r.Tag)
			}
		}
		if len(releases) < 100 {
			return tags, nil
		}
	}
	return nil, errors.New("GitHub release list exceeded the check limit")
}
