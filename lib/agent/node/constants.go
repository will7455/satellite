package node

import "time"

// MemberStatus describes the state of a serf node.
type MemberStatus string

// Defines possible member status.
const (
	MemberAlive   MemberStatus = "alive"
	MemberLeaving              = "leaving"
	MemberLeft                 = "left"
	MemberFailed               = "failed"
)

// Role describes the agent's server role.
type Role string

// Defines possible roles.
const (
	RoleMaster Role = "master"
	RoleNode        = "node"
)

const (
	// recycleCacheInterval is the amount of time to wait between recycle attempts.
	// Recycle is a request to clean up / remove stale data that backends can
	// choose to implement.
	recycleCacheInterval = 10 * time.Minute

	// updateClusterStatusInterval is the amount of time to wait between cluster
	// status update collections.
	updateClusterStatusInterval = 30 * time.Second

	// pushEventsInterval is the amount of time to wait between attempting to
	// push new local timeline events to master nodes.
	pushEventsInterval = 30 * time.Second

	// updateLocalStatusInterval is the amount of time to wait between local
	// status update collections.
	updateLocalStatusInterval = updateClusterStatusInterval

	// checkerTimeout is the max amount of time to wait for a checker to report
	// results.
	checkerTimeout = updateLocalStatusInterval - (5 * time.Second)

	// probeTimeout is the max amount of time to wait for check to return probes.
	probeTimeout = checkerTimeout - (5 * time.Second)

	// timelineInitTimeout specifies the amount of time to wait for the
	// timeline to initialize.
	timelineInitTimeout = time.Minute
)

// maxConcurrentCheckers specifies the maximum number of checkers active at any
// given time.
const maxConcurrentCheckers = 10
