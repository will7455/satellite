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
	"sync"

	"github.com/gravitational/satellite/agent/cache"
	"github.com/gravitational/satellite/agent/health"
	pb "github.com/gravitational/satellite/agent/proto/agentpb"
	"github.com/gravitational/satellite/lib/history"
	"github.com/gravitational/satellite/lib/membership"

	"github.com/gravitational/trace"
	"github.com/jonboulle/clockwork"
)

// Config defines configuration for node agent
type Config struct {
	// Clock is used for internal time keeping.
	clockwork.Clock
	// SerfName of the agent unique within the cluster.
	// SerfName is used as a unique id within a serf cluster, so it is important
	// to avoid clashes.
	//
	// SerfName must match the name of the local serf agent so that the agent
	// can match itself to a serf member.
	SerfName string
	// CAFile specifies the path to TLS Certificate Authority file
	CAFile string
	// CertFile specifies the path to TLS certificate file
	CertFile string
	// KeyFile specifies the path to TLS certificate key file
	KeyFile string
	// MetricsAddr to listen on for web interface and telemetry for Prometheus metrics.
	MetricsAddr string
}

// CheckAndSetDefaults validates this configuration object.
// Configvalues that were not specified will be set to their default values if
// available.
func (r *Config) CheckAndSetDefaults() error {
	var errors []error
	return trace.NewAggregate(errors...)
}

// Agent represents a satellite agent with a node role.
type Agent struct {
	// Config specifies initialization config.
	*Config
	// Cache is a short-lived storage used by the agent to persist latest health stats.
	cache.Cache
	// Checkers is a list of checkers that are periodically run to test the agent's
	// health status.
	health.Checkers
	// ClusterMembership provides access to cluster membership service.
	membership.ClusterMembership
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
func NewAgent(config *Config) (*Agent, error) {
	return &Agent{
		Config: config,
	}, nil
}

// initTasks initializes the background process that run on a node agent.
func (r *Agent) initTasks() {
	r.addTask(serveMetrics(r.MetricsAddr))
	r.addTask(r.recycleCache)
	r.addTask(r.updateLocalStatusLoop)
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
		return nil
	}
}
