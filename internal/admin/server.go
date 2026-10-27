package admin

import (
	"context"
	"errors"
	"io/fs"
	"net/http"
	"runtime"
	"syscall"
	"time"

	"github.com/seanmmitchell/gos3/internal/config"
	"github.com/seanmmitchell/gos3/internal/discovery"
	"github.com/seanmmitchell/gos3/internal/metrics"
	"github.com/seanmmitchell/gos3/internal/storage"
)

// serverInfo is the GET /info response: Info plus host details.
type serverInfo struct {
	discovery.Info
	Backend       string `json:"backend"`
	DataPath      string `json:"dataPath"`
	GoVersion     string `json:"goVersion"`
	OS            string `json:"os"`
	Arch          string `json:"arch"`
	NumCPU        int    `json:"numCPU"`
	UptimeSeconds int64  `json:"uptimeSeconds"`
}

func (a *api) serverInfo(rc *reqCtx) (int, any, error) {
	inf := a.info()
	return http.StatusOK, serverInfo{
		Info:          inf,
		Backend:       a.d.BackendName,
		DataPath:      a.d.DataPath,
		GoVersion:     runtime.Version(),
		OS:            runtime.GOOS,
		Arch:          runtime.GOARCH,
		NumCPU:        runtime.NumCPU(),
		UptimeSeconds: a.uptimeSeconds(inf),
	}, nil
}

// uptimeSeconds is measured from Info.StartedAt (or handler creation).
func (a *api) uptimeSeconds(inf discovery.Info) int64 {
	start := inf.StartedAt
	if start.IsZero() {
		start = a.started
	}
	up := int64(a.now().Sub(start) / time.Second)
	return max(up, 0)
}

// usageJSON is storage.BucketUsage with the API's JSON names.
type usageJSON struct {
	Objects            int64 `json:"objects"`
	Bytes              int64 `json:"bytes"`
	StoredBytes        int64 `json:"storedBytes"`
	Versions           int64 `json:"versions"`
	VersionBytes       int64 `json:"versionBytes"`
	StoredVersionBytes int64 `json:"storedVersionBytes"`
	CompressedVersions int64 `json:"compressedVersions"`
	DeleteMarkers      int64 `json:"deleteMarkers"`
	PendingUploads     int64 `json:"pendingUploads"`
}

// bucketUsageJSON is the usage of a BucketSummary: the counters plus when
// they were computed (usage is cached for up to usageTTL, so a client can
// tell how current the figures are).
type bucketUsageJSON struct {
	usageJSON
	ComputedAt time.Time `json:"computedAt"`
}

func toUsageJSON(u storage.BucketUsage) *usageJSON {
	return &usageJSON{
		Objects:            u.Objects,
		Bytes:              u.Bytes,
		StoredBytes:        u.StoredBytes,
		Versions:           u.Versions,
		VersionBytes:       u.VersionBytes,
		StoredVersionBytes: u.StoredVersionBytes,
		CompressedVersions: u.CompressedVersions,
		DeleteMarkers:      u.DeleteMarkers,
		PendingUploads:     u.PendingUploads,
	}
}

// storageTotals is the "storage" section of GET /stats.
type storageTotals struct {
	Buckets int64 `json:"buckets"`
	usageJSON
	ComputedAt time.Time `json:"computedAt"`
}

type runtimeStats struct {
	Goroutines     int    `json:"goroutines"`
	HeapAllocBytes uint64 `json:"heapAllocBytes"`
	SysBytes       uint64 `json:"sysBytes"`
	GCPauseTotalNs uint64 `json:"gcPauseTotalNs"`
}

// diskStats describes the file system holding the data directory.
type diskStats struct {
	Path       string `json:"path"`
	TotalBytes uint64 `json:"totalBytes"`
	FreeBytes  uint64 `json:"freeBytes"`
	UsedBytes  uint64 `json:"usedBytes"`
}

type statsResponse struct {
	Metrics metrics.Snapshot `json:"metrics"`
	Storage storageTotals    `json:"storage"`
	Runtime runtimeStats     `json:"runtime"`
	Disk    *diskStats       `json:"disk"`
}

func (a *api) stats(rc *reqCtx) (int, any, error) {
	totals, err := a.storageTotals(rc.ctx)
	if err != nil {
		return 0, nil, err
	}
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	resp := statsResponse{
		Metrics: a.metricsSnapshot(),
		Storage: totals,
		Runtime: runtimeStats{
			Goroutines:     runtime.NumGoroutine(),
			HeapAllocBytes: ms.HeapAlloc,
			SysBytes:       ms.Sys,
			GCPauseTotalNs: ms.PauseTotalNs,
		},
	}
	if a.d.BackendName == "fs" && a.d.DataPath != "" {
		resp.Disk = diskUsage(a.d.DataPath)
	}
	return http.StatusOK, resp, nil
}

// metricsSnapshot returns the request metrics; without a registry it is an
// empty snapshot (never null, so clients need no special case).
func (a *api) metricsSnapshot() metrics.Snapshot {
	if a.d.Metrics != nil {
		s := a.d.Metrics.Snapshot()
		if s.Ops == nil {
			s.Ops = []metrics.OpStats{}
		}
		if s.Recent == nil {
			s.Recent = []metrics.MinuteBucket{}
		}
		return s
	}
	start := a.info().StartedAt
	if start.IsZero() {
		start = a.started
	}
	return metrics.Snapshot{
		StartedAt:     start,
		UptimeSeconds: a.uptimeSeconds(a.info()),
		Ops:           []metrics.OpStats{},
		Recent:        []metrics.MinuteBucket{},
	}
}

// storageTotals sums usage over all buckets (cached for usageTTL, with
// concurrent requests sharing one computation). ComputedAt is the time of
// the oldest per-bucket figure that went into the sum: those come from the
// per-bucket cache, so the totals can be older than the moment they were
// added up.
func (a *api) storageTotals(ctx context.Context) (storageTotals, error) {
	t, _, err := a.totals.get(ctx, "", func(ctx context.Context) (storageTotals, error) {
		buckets, err := a.d.Backend.ListBuckets(ctx)
		if err != nil {
			return storageTotals{}, err
		}
		var t storageTotals
		oldest := a.now()
		for _, b := range buckets {
			u, at, err := a.bucketUsage(ctx, b.Name)
			if errors.Is(err, storage.ErrBucketNotFound) {
				continue // deleted concurrently
			}
			if err != nil {
				return storageTotals{}, err
			}
			if at.Before(oldest) {
				oldest = at
			}
			t.Buckets++
			t.Objects += u.Objects
			t.Bytes += u.Bytes
			t.StoredBytes += u.StoredBytes
			t.Versions += u.Versions
			t.VersionBytes += u.VersionBytes
			t.StoredVersionBytes += u.StoredVersionBytes
			t.CompressedVersions += u.CompressedVersions
			t.DeleteMarkers += u.DeleteMarkers
			t.PendingUploads += u.PendingUploads
		}
		t.ComputedAt = oldest.UTC()
		return t, nil
	})
	return t, err
}

// bucketUsage returns the (cached) usage of one bucket and when it was
// computed.
func (a *api) bucketUsage(ctx context.Context, bucket string) (storage.BucketUsage, time.Time, error) {
	return a.usage.get(ctx, bucket, func(ctx context.Context) (storage.BucketUsage, error) {
		return storage.ComputeBucketUsage(ctx, a.d.Backend, bucket)
	})
}

// invalidateUsage forgets cached usage of bucket and the totals.
func (a *api) invalidateUsage(bucket string) {
	a.usage.invalidate(bucket)
	a.totals.invalidateAll()
}

func (a *api) getConfig(rc *reqCtx) (int, any, error) {
	return http.StatusOK, normalizeSettings(a.d.Settings.Get()), nil
}

func (a *api) putConfig(rc *reqCtx) (int, any, error) {
	// Clients predating compression or updates preserve those sections
	// when omitted. Fields they send replace the current values; every
	// other section must be complete.
	current := a.d.Settings.Get()
	in := config.Settings{Compression: current.Compression, Updates: current.Updates}
	if err := rc.decodeBody(&in, false); err != nil {
		var ae *apiError
		if errors.As(err, &ae) && ae.Code == codeInvalidRequest && ae.Status == http.StatusBadRequest {
			ae.Code = codeInvalidSettings
		}
		return 0, nil, err
	}
	// config.ValidateSettings is the single source of truth; Store.Update
	// validates again, but checking first keeps the 400 independent of how
	// the store reports errors.
	if err := config.ValidateSettings(in); err != nil {
		return 0, nil, errorf(http.StatusBadRequest, codeInvalidSettings, "%s", err.Error())
	}
	out, err := a.d.Settings.Update(func(s *config.Settings) error {
		*s = in
		return nil
	})
	if err != nil {
		// The store validates and persists: file system failures are server
		// errors, anything else is a rejected document.
		var pe *fs.PathError
		var errno syscall.Errno
		if errors.As(err, &pe) || errors.As(err, &errno) {
			return 0, nil, errorf(http.StatusInternalServerError, codeInternalError, "saving settings: %v", err)
		}
		return 0, nil, errorf(http.StatusBadRequest, codeInvalidSettings, "%v", err)
	}
	a.log.Info("admin: settings updated", "user", rc.id.User)
	return http.StatusOK, normalizeSettings(out), nil
}

// normalizeSettings replaces nil slices with empty ones for stable JSON.
func normalizeSettings(s config.Settings) config.Settings {
	nn := func(v []string) []string {
		if v == nil {
			return []string{}
		}
		return v
	}
	s.Domains = nn(s.Domains)
	s.Updates.NotifyFor = nn(s.Updates.NotifyFor)
	s.CORS.AllowedOrigins = nn(s.CORS.AllowedOrigins)
	s.CORS.AllowedHeaders = nn(s.CORS.AllowedHeaders)
	s.CORS.ExposeHeaders = nn(s.CORS.ExposeHeaders)
	s.Discovery.Seeds = nn(s.Discovery.Seeds)
	s.Discovery.DNSSRV = nn(s.Discovery.DNSSRV)
	s.Compression.SkipContentTypes = nn(s.Compression.SkipContentTypes)
	return s
}
