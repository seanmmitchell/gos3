package cluster

import "errors"

// ErrConsensusUnavailable reports that Raft stopped or rejected a proposal
// before a quorum-confirmed result was available.
var ErrConsensusUnavailable = errors.New("cluster: consensus unavailable")

// ErrVerifiedReplicaUnavailable reports that no selected replica could
// provide the bytes matching a committed version's digest.
var ErrVerifiedReplicaUnavailable = errors.New("cluster: verified replica unavailable")
