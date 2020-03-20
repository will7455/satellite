/*
Copyright 2020 Gravitational, Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package node

import (
	"context"
	"path"
	"sync"
	"time"

	"github.com/gravitational/satellite/agent/cache"
	"github.com/gravitational/satellite/agent/health"
	pb "github.com/gravitational/satellite/agent/proto/agentpb"
	"github.com/gravitational/satellite/lib/history"
	"github.com/gravitational/satellite/lib/history/sqlite"
	"github.com/gravitational/satellite/lib/membership"
	"github.com/gravitational/satellite/lib/rpc"

	"github.com/gravitational/trace"
	serf "github.com/hashicorp/serf/client"
	"github.com/jonboulle/clockwork"
	log "github.com/sirupsen/logrus"
)

// Config defines configuration for node agent
type Config struct {
	// Clock is used for internal time keeping.
	clockwork.Clock
	// Cache is a short-lived storage used by the agent to persist latest health stats.
	cache.Cache
	// SerfName of the agent unique within the cluster.
	// SerfName is used as a unique id within a serf cluster, so it is important
	// to avoid clashes.
	//
	// SerfName must match the name of the local serf agent so that the agent
	// can match itself to a serf member.
	SerfName string
	// RPCConfig specifies rpc configuration.
	RPCConfig rpc.Config
	// MetricsAddr to listen on for web interface and telemetry for Prometheus metrics.
	MetricsAddr string
	// TimelineConfig specifies sqlite timeline configuration.
	TimelineConfig sqlite.Config
	// SerfConfig specifies serf client configuration.
	SerfConfig serf.Config
}

// CheckAndSetDefaults validates this configuration object.
// Configvalues that were not specified will be set to their default values if
// available.
func (r *Config) CheckAndSetDefaults() error {
	var errors []error
	if err := (&r.RPCConfig).CheckAndSetDefaults(); err != nil {
		errors = append(errors, err)
	}
	if r.SerfName == "" {
		errors = append(errors, trace.BadParameter("agent serf name cannot be empty"))
	}
	if r.Clock == nil {
		r.Clock = clockwork.NewRealClock()
	}
	return trace.NewAggregate(errors...)
}

// Agent represents a satellite agent with a node role.
type Agent struct {
	// Config specifies initialization config.
	Config
	// Checkers is a list of checkers that are periodically run to test the agent's
	// health status.
	health.Checkers
	// Tasks is a list of background processes that are run by the agent.
	Tasks []Task
	// LocalTimeline keeps track of local timeline events.
	LocalTimeline history.Timeline

	// Mutex locks access to Agent
	sync.Mutex
	//localStatus is the last obtained local node status.
	localStatus *pb.NodeStatus
}

// NewAgent constructs a new Agent with the provided config.
func NewAgent(config Config) (*Agent, error) {
	if err := (&config).CheckAndSetDefaults(); err != nil {
		return nil, trace.Wrap(err)
	}

	localTimeline, err := initTimeline(config.TimelineConfig, "local.db")
	if err != nil {
		return nil, trace.Wrap(err, "failed to initialize local timeline")
	}

	return &Agent{
		Config:        config,
		LocalTimeline: localTimeline,
		localStatus:   emptyNodeStatus(config.SerfName),
	}, nil
}

// initTimeline initializes a new sqlite timeline. dbName specifies the
// SQLite database file name.
func initTimeline(config sqlite.Config, fileName string) (history.Timeline, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timelineInitTimeout)
	defer cancel()
	config.DBPath = path.Join(config.DBPath, fileName)
	return sqlite.NewTimeline(ctx, config)
}

// initSerfTags updates serf member tags with the provided tags.
func initSerfTags(config serf.Config, tags map[string]string) error {
	client, err := membership.NewSerfClient(config)
	if err != nil {
		return trace.Wrap(err, "failed to connect to serf cluster")
	}

	defer func() {
		if err := client.Close(); err != nil {
			log.WithError(err).Error("Failed to close serf client")
		}
	}()

	if err = client.UpdateTags(tags, nil); err != nil {
		return trace.Wrap(err, "failed to update serf agent tags")
	}
	return nil
}

// initTasks initializes the background process that run on a node agent.
func (r *Agent) initTasks() {
	r.addTask(serveMetrics(r.MetricsAddr))
	r.addTask(r.recycleCacheTask)
	r.addTask(r.updateLocalStatusTask)
	r.addTask(r.updateClusterStatusTask)
}

// addTask appends the task to the agent's list of background processes.
func (r *Agent) addTask(task Task) {
	r.Tasks = append(r.Tasks, task)
}

// Run runs the agent's background tasks.
func (r *Agent) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(len(r.Tasks))
	waitCh := make(chan struct{})
	errCh := make(chan error, len(r.Tasks))
	for _, task := range r.Tasks {
		go func(task Task) {
			defer wg.Done()
			errCh <- task(ctx)
		}(task)
	}

	go func() {
		wg.Wait()
		close(waitCh)
	}()

	// Return on first error, or wait for all tasks to complete.
	select {
	case err := <-errCh:
		return trace.Wrap(err, "A background process returned an error.")
	case <-waitCh:
		log.Info("Successfully shutdown all background processes.")
		return nil
	}
}

// IsMember returns true if this agent is a member of a serf cluster.
func (r *Agent) IsMember() bool {
	client, err := membership.NewSerfClient(r.SerfConfig)
	if err != nil {
		log.WithError(err).Error("Failed to connect to serf cluster.")
		return false
	}

	defer func() {
		if err := client.Close(); err != nil {
			log.WithError(err).Warn("Failed to close serf client")
		}
	}()

	members, err := client.Members()
	if err != nil {
		log.WithError(err).Error("Failed to retrieve members.")
		return false
	}

	return r.isMember(members)
}

func (r *Agent) isMember(members []membership.ClusterMember) bool {
	// if we're the only one, consider that we're not in a cluster yet
	// (cause more often than not there is more than 1 member)
	if len(members) == 1 && members[0].Name() == r.SerfName {
		return false
	}
	for _, member := range members {
		if member.Name() == r.SerfName {
			return true
		}
	}
	return false
}

// Join attempts to join a serf cluster identified by peers.
func (r *Agent) Join(peers []string) error {
	client, err := membership.NewSerfClient(r.SerfConfig)
	if err != nil {
		return trace.Wrap(err, "failed to connect to serf cluster")
	}

	defer func() {
		if err := client.Close(); err != nil {
			log.WithError(err).Warn("Failed to close serf client")
		}
	}()

	noReplay := false
	numJoined, err := client.Join(peers, noReplay)
	if err != nil {
		return trace.Wrap(err)
	}

	log.Infof("joined %d nodes", numJoined)
	return nil
}

// Time reports the current server time.
func (r *Agent) Time() time.Time {
	return r.Clock.Now()
}

// Status returns the list known cluster status.
func (r *Agent) Status() (status *pb.SystemStatus, err error) {
	status, err = r.Cache.RecentStatus()
	if err == nil && status == nil {
		status = pb.EmptyStatus()
	}
	return status, trace.Wrap(err)
}

// LocalStatus reports the status of the local agent node.
func (r *Agent) LocalStatus() *pb.NodeStatus {
	return r.getLocalStatus()
}

// getLocalStatus returns the last known local node status.
func (r *Agent) getLocalStatus() *pb.NodeStatus {
	r.Lock()
	defer r.Unlock()
	return r.localStatus
}

// setLocalStatus sets replaces the previous localStatus we a new status.
func (r *Agent) setLocalStatus(status *pb.NodeStatus) {
	r.Lock()
	defer r.Unlock()
	r.localStatus = status
}
