package server

import (
	"errors"
	"fmt"
	"path/filepath"

	"github.com/seanmmitchell/gos3/internal/config"
	"github.com/seanmmitchell/gos3/internal/copyjob"
	"github.com/seanmmitchell/gos3/internal/jobs"
	"github.com/seanmmitchell/gos3/internal/resticsnap"
	"github.com/seanmmitchell/gos3/internal/storage/multi"
	"github.com/seanmmitchell/gos3/internal/storage/restic"
)

// Background jobs (docs/jobs.md, docs/ARCHITECTURE.md "Jobs, copies and
// restic snapshots").
//
// The server owns the jobs Manager (internal/jobs) and the two services
// whose runners it runs: copy and mirror jobs (internal/copyjob) and
// whole-bucket restic snapshots, restores and prunes (internal/resticsnap,
// whose scheduler submits the snapshot jobs). They are opened in New, once
// the router and the runtime settings are, and are handed to the admin API.
// Jobs start running as soon as the Manager is open, also before the
// listener serves (a restart resumes the jobs that were running).
//
// State in the system directory: jobs/ (jobs.json and the jobs' state
// directories; JobsDirName), restic-snapshots.json (the scheduler's state)
// and restic-stage/ (the default stage of snapshot runs). Without a system
// directory the jobs are kept in memory (a warning is logged) and the stage
// is in the temporary directory.
//
// Shutdown (Close) stops them before the router: the snapshot scheduler
// first (it submits jobs), then the copy job service (it kicks mirrors
// from the router's change feed), then the Manager, which cancels the
// running jobs and waits for them, so that no job uses the router while it
// closes.

// JobsDirName is the directory in the system directory holding the jobs
// file (jobs.json) and the jobs' state directories.
const JobsDirName = "jobs"

// openJobs creates the copy job and snapshot services and the jobs Manager
// running their runners, and attaches the services to it. An unreadable
// jobs file (another format version, damaged) fails it.
func (s *Server) openJobs() error {
	router, ok := s.backend.(*multi.Router)
	if !ok {
		return errors.New("background jobs need the storage router")
	}
	cfg := s.cfg
	log := s.log.With("component", "jobs")

	s.copyJobs = copyjob.New(copyjob.Options{
		Router:      router,
		Region:      func() string { return s.settings.Get().Region },
		Logger:      log,
		Compression: func() config.CompressionSettings { return s.settings.Get().Compression },
	})
	snap := cfg.Restic.Snapshots
	snaps, err := resticsnap.New(resticsnap.Options{
		Router: router,
		// Snapshot runs only run restic commands against repositories
		// (backups of the stage, listings, dumps, forgets, prunes); the
		// per-version archive of each backing keeps its own Runtime.
		Runtime:     &restic.Runtime{Log: s.log.With("component", "restic-snapshots")},
		SystemDir:   s.systemDir,
		StageDir:    snap.StageDir,
		SpoolBytes:  snap.SpoolBytes,
		Incremental: snap.Incremental,
		Workers:     snap.Workers,
		Logger:      s.log.With("component", "restic-snapshots"),
	})
	if err != nil {
		return fmt.Errorf("opening the restic snapshot scheduler: %w", err)
	}
	s.snapshots = snaps

	runners := map[string]jobs.Runner{}
	for _, kind := range []string{copyjob.KindCopy, copyjob.KindMirror} {
		runners[kind] = s.copyJobs.Runner(kind)
	}
	for _, kind := range []string{resticsnap.KindSnapshot, resticsnap.KindRestore, resticsnap.KindPrune} {
		runners[kind] = snaps.Runner(kind)
	}
	// Jobs are dispatched only once the copy runner and the snapshot
	// scheduler know the manager (copy jobs check other jobs when they
	// start).
	m, err := jobs.Open(jobs.Options{
		Dir:        s.jobsDir(),
		Workers:    cfg.Jobs.Workers,
		Runners:    runners,
		Logger:     log,
		DeferStart: true,
	})
	if err != nil {
		return fmt.Errorf("opening background jobs: %w", err)
	}
	s.jobs = m
	s.copyJobs.Attach(m)
	snaps.Start(m)
	m.Start()
	s.log.Info("background jobs", "dir", s.jobsDir(), "workers", cfg.Jobs.Workers,
		"snapshot_workers", snap.Workers, "stage_dir", s.SnapshotStageDir(), "incremental", snap.Incremental)
	return nil
}

// closeJobs stops the background jobs (see the top of the file). It is
// part of Close and safe on a partially opened server.
func (s *Server) closeJobs() error {
	if s.snapshots != nil {
		s.snapshots.Stop()
	}
	if s.copyJobs != nil {
		s.copyJobs.Close()
	}
	if s.jobs != nil {
		if err := s.jobs.Close(); err != nil {
			return fmt.Errorf("saving background jobs: %w", err)
		}
	}
	return nil
}

// jobsDir returns the directory of the jobs ("" when they are kept in
// memory).
func (s *Server) jobsDir() string {
	if s.systemDir == "" {
		return ""
	}
	return filepath.Join(s.systemDir, JobsDirName)
}

// Jobs returns the background job Manager (nil before New completed).
func (s *Server) Jobs() *jobs.Manager { return s.jobs }

// JobsDir returns the directory holding jobs.json and the jobs' state ("":
// the jobs are kept in memory and lost when the server stops).
func (s *Server) JobsDir() string { return s.jobsDir() }

// JobsWorkers returns the number of background jobs that run at once
// (jobs.workers).
func (s *Server) JobsWorkers() int { return s.cfg.Jobs.Workers }

// SnapshotStageDir returns the stage directory of whole-bucket snapshot
// runs: restic.snapshots.stage_dir, by default restic-stage in the system
// directory. Without either it returns "": the snapshot scheduler then uses
// a private temporary directory of its own, created on first use and
// removed at shutdown.
func (s *Server) SnapshotStageDir() string {
	if d := s.cfg.Restic.Snapshots.StageDir; d != "" {
		return d
	}
	if s.systemDir != "" {
		return filepath.Join(s.systemDir, "restic-stage")
	}
	return ""
}
