package s3store

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Remote layout. Every key below is relative to P = <Bucket>/<Prefix>:
//
//	gos3.json                              marker (marker.go)
//	meta/probe-<id>                        conditional-write probe (deleted again)
//	meta/lease.json                        owner lease (lease.go)
//	meta/journal/<epoch:016x>/<seq:016x>   journal batches and seals (journal.go)
//	meta/snapshots/<lsn:016x>-<epoch:016x> metadata snapshots (snapshot.go)
//	data/, parts/, keys/, restic/, gateway/ object data and keys (backend)
//
// Numbers in keys are fixed-width lower-case hex, so the lexical order of
// keys is their numeric order.
const (
	markerKey      = "gos3.json"
	metaDir        = "meta/"
	probePrefix    = "meta/probe-"
	leaseKey       = "meta/lease.json"
	journalDir     = "meta/journal/"
	snapshotDir    = "meta/snapshots/"
	dataDir        = "data/"
	keysDir        = "keys/"
	markerFormat   = 1
	markerKind     = "s3"
	journalVersion = 1
	snapVersion    = 1
)

// Errors of the metadata engine, besides those in options.go.
var (
	// ErrUnmanagedPrefix means the prefix holds backing data (meta/, data/
	// or keys/ objects) but no gos3.json marker, so it is not opened (and
	// no marker is created over it).
	ErrUnmanagedPrefix = errors.New("s3store: prefix holds gos3 data but no gos3.json marker")
	// ErrMarkerMismatch means the prefix's gos3.json marker does not
	// describe a backing these options can open (unknown format or kind,
	// or a different gateway setting).
	ErrMarkerMismatch = errors.New("s3store: gos3.json marker does not match the backing options")
	// ErrCorruptJournal means the journal or a snapshot cannot be replayed
	// (a missing range of records, a checksum mismatch, an undecodable
	// object).
	ErrCorruptJournal = errors.New("s3store: metadata journal is damaged")
)

// keys resolves layout keys under one bucket prefix.
type keys struct {
	prefix string // "" or ends with "/"
}

func (k keys) marker() string { return k.prefix + markerKey }
func (k keys) lease() string  { return k.prefix + leaseKey }
func (k keys) probe(id string) string {
	return k.prefix + probePrefix + id
}
func (k keys) journalRoot() string { return k.prefix + journalDir }
func (k keys) journalEpoch(epoch uint64) string {
	return k.prefix + journalDir + hex16(epoch) + "/"
}
func (k keys) batch(epoch, seq uint64) string {
	return k.journalEpoch(epoch) + hex16(seq)
}
func (k keys) snapshotRoot() string { return k.prefix + snapshotDir }
func (k keys) snapshot(lsn, epoch uint64) string {
	return k.prefix + snapshotDir + hex16(lsn) + "-" + hex16(epoch)
}

// parseBatch parses a journal batch key (with the prefix).
func (k keys) parseBatch(key string) (epoch, seq uint64, ok bool) {
	rest, found := strings.CutPrefix(key, k.prefix+journalDir)
	if !found {
		return 0, 0, false
	}
	e, s, found := strings.Cut(rest, "/")
	if !found {
		return 0, 0, false
	}
	epoch, ok1 := parseHex16(e)
	seq, ok2 := parseHex16(s)
	return epoch, seq, ok1 && ok2
}

// parseSnapshot parses a snapshot key (with the prefix).
func (k keys) parseSnapshot(key string) (lsn, epoch uint64, ok bool) {
	rest, found := strings.CutPrefix(key, k.prefix+snapshotDir)
	if !found {
		return 0, 0, false
	}
	l, e, found := strings.Cut(rest, "-")
	if !found {
		return 0, 0, false
	}
	lsn, ok1 := parseHex16(l)
	epoch, ok2 := parseHex16(e)
	return lsn, epoch, ok1 && ok2
}

func hex16(n uint64) string { return fmt.Sprintf("%016x", n) }

func parseHex16(s string) (uint64, bool) {
	if len(s) != 16 {
		return 0, false
	}
	n, err := strconv.ParseUint(s, 16, 64)
	return n, err == nil
}
