package node

import (
	"context"
	"net"
	"net/http"
	"runtime/debug"

	"github.com/gravitational/satellite/agent/health"
	pb "github.com/gravitational/satellite/agent/proto/agentpb"
	"github.com/gravitational/satellite/lib/history"
	"github.com/gravitational/satellite/lib/membership"
	netUtil "github.com/gravitational/satellite/utils/net"

	"github.com/gravitational/trace"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	log "github.com/sirupsen/logrus"
)

// Task represents a background process running on the agent.
type Task func(context.Context) error

// serveMetrics returns a Task that serves and closes a metrics listener
// listening on the provided addr.
func serveMetrics(addr string) Task {
	return func(ctx context.Context) error {
		ctx, cancel := context.WithCancel(ctx)
		defer cancel()

		listener, err := net.Listen("tcp", addr)
		if err != nil {
			return trace.Wrap(err, "failed to initialize metrics listener")
		}

		// Close metrics listener when context is done
		go func() {
			<-ctx.Done()
			if err := listener.Close(); err != nil {
				log.WithError(err).Error("Failed to close metrics listener.")
			}
		}()

		http.Handle("/metrics", promhttp.Handler())
		if err = http.Serve(listener, nil); err != nil && !netUtil.IsProbableEOF(err) {
			log.WithError(err).Error("Failed to serve metrics listener.")
			return trace.Wrap(err)
		}

		log.Info("Metrics listener has been closed.")
		return nil
	}
}

// recycleCacheTask is a background process that periodically recycles the cache.
func (r *Agent) recycleCacheTask(ctx context.Context) error {
	ticker := r.Clock.NewTicker(recycleCacheInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Info("Recycle loop is stopping.")
			return nil
		case <-ticker.Chan():
			if err := r.Cache.Recycle(); err != nil {
				log.WithError(err).Warn("Error recycling status.")
			}
		}
	}
}

// updateClusterStatusTask is a background process that periodically updates the health
// status of the cluster.
func (r *Agent) updateClusterStatusTask(ctx context.Context) error {
	ticker := r.Clock.NewTicker(updateClusterStatusInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Info("Cluster status update loop is stopping.")
			return nil
		case <-ticker.Chan():
			if err := r.updateClusterStatus(ctx); err != nil {
				log.WithError(err).Warn("Failed to update status.")
			}
		}
	}
}

// updateClusterStatus updates the cluster health status.
func (r *Agent) updateClusterStatus(ctx context.Context) error {
	status, err := r.collectClusterStatus(ctx)
	if err != nil {
		return trace.Wrap(err, "failed to collect cluster status")
	}
	if err := r.Cache.UpdateStatus(status); err != nil {
		return trace.Wrap(err, "failed to update cache with cluster status")
	}
	return nil
}

// collectClusterStatus collects the cluster status.
func (r *Agent) collectClusterStatus(ctx context.Context) (*pb.SystemStatus, error) {
	client, err := membership.NewSerfClient(r.SerfConfig)
	if err != nil {
		return nil, trace.Wrap(err, "failed to connect to serf cluster")
	}

	defer func() {
		if err := client.Close(); err != nil {
			log.WithError(err).Warn("Failed to close serf client")
		}
	}()

	members, err := client.Members()
	if err != nil {
		return nil, trace.Wrap(err)
	}

	log.WithField("members", members).Debug("Started collecting statuses from members.")

	// Start collecting status from cluster members.
	statusCh := make(chan *pb.NodeStatus, len(members))
	for _, member := range members {
		go func(member membership.ClusterMember) {
			status, err := r.getStatusFrom(ctx, member)
			if err != nil {
				log.WithError(err).Warnf("Failed to query node %s(%v) status.", member.Name(), member.Addr())
				statusCh <- unknownNodeStatus(member)
				return
			}
			log.WithField("status", status).Debug("Retreived status.")
			statusCh <- status
		}(member)
	}

	// Collect node statuses.
	// If context is done return currently collected node statuses.
	nodes := make([]*pb.NodeStatus, 0, len(members))
	for i := 0; i < len(members); i++ {
		select {
		case status := <-statusCh:
			nodes = append(nodes, status)
		case <-ctx.Done():
			log.WithError(ctx.Err()).Warn("Timed out collecting node statuses.")
			break
		}
	}

	systemStatus := &pb.SystemStatus{
		Status:    pb.SystemStatus_Unknown,
		Timestamp: pb.NewTimeToProto(r.Clock.Now()),
		Nodes:     nodes,
	}
	setSystemStatus(systemStatus, members)

	return systemStatus, nil
}

// getStatusFrom collects node status from the specified member.
func (r *Agent) getStatusFrom(ctx context.Context, member membership.ClusterMember) (*pb.NodeStatus, error) {
	client, err := member.Dial(ctx, r.RPCConfig.CAFile, r.RPCConfig.CertFile, r.RPCConfig.KeyFile)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	defer client.Close()

	return client.LocalStatus(ctx)
}

// pushEventsTask is a background process that periodically pushes new local
// timeline events to all master nodes.
func (r *Agent) pushEventsTask(ctx context.Context) error {
	ticker := r.Clock.NewTicker(pushEventsInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Info("Push events task is stopping.")
			return nil
		case <-ticker.Chan():
			if err := r.notifyMasters(ctx); err != nil {
				log.WithError(err).Warn("Failed to notify master nodes of new local timeline events.")
			}
		}
	}
}

// notifyMasters notifies the master nodes in the cluster of new local timeline
// events.
func (r *Agent) notifyMasters(ctx context.Context) error {
	client, err := membership.NewSerfClient(r.SerfConfig)
	if err != nil {
		return trace.Wrap(err, "failed to connect to serf cluster")
	}

	defer func() {
		if err := client.Close(); err != nil {
			log.WithError(err).Warn("Failed to close serf client")
		}
	}()

	members, err := client.Members()
	if err != nil {
		return trace.Wrap(err)
	}

	events, err := r.LocalTimeline.GetEvents(ctx, nil)
	if err != nil {
		return trace.Wrap(err)
	}

	for _, member := range members {
		if !hasRoleMaster(member.Tags()) {
			continue
		}
		go func(member membership.ClusterMember) {
			if err := r.notifyMaster(ctx, member, events); err != nil {
				log.WithError(err).Warnf("Failed to notify %s of new timeline events.", member.Name())
			}
		}(member)
	}
	return nil
}

// notifyMaster notifies the speified member of the events.
func (r *Agent) notifyMaster(ctx context.Context, member membership.ClusterMember, events []*pb.TimelineEvent) error {
	client, err := member.Dial(ctx, r.RPCConfig.CAFile, r.RPCConfig.CertFile, r.RPCConfig.KeyFile)
	if err != nil {
		return trace.Wrap(err)
	}
	defer client.Close()

	resp, err := client.LastSeen(ctx, &pb.LastSeenRequest{Name: r.SerfName})
	if err != nil {
		return trace.Wrap(err)
	}

	// Filter out previously recorded events.
	filtered := filterByTimestamp(events, resp.GetTimestamp().ToTime())

	for _, event := range filtered {
		if _, err := client.UpdateTimeline(ctx, &pb.UpdateRequest{Name: r.SerfName, Event: event}); err != nil {
			return trace.Wrap(err)
		}
	}

	return nil
}

// updateLocalStatusTask is a background process that periodically updates the
// agent's local health status.
func (r *Agent) updateLocalStatusTask(ctx context.Context) error {
	ticker := r.Clock.NewTicker(updateLocalStatusInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Info("Local status update loop is stopping.")
			return nil
		case <-ticker.Chan():
			if err := r.updateLocalStatus(ctx); err != nil {
				log.WithError(err).Warn("Failed to update local status.")
			}
		}
	}
}

// updateLocalStatus updates the local status.
func (r *Agent) updateLocalStatus(ctx context.Context) error {
	status, err := r.collectLocalStatus(ctx)
	if err != nil {
		return trace.Wrap(err, "failed to collect local status")
	}

	events := history.DiffNode(r.Clock, r.getLocalStatus(), status)
	if err := r.LocalTimeline.RecordEvents(ctx, events); err != nil {
		return trace.Wrap(err, "failed to record local timeline events")
	}

	r.setLocalStatus(status)

	return nil
}

// collectLocalStatus runs the agent's health checks and returns the resulting
// status.
func (r *Agent) collectLocalStatus(ctx context.Context) (*pb.NodeStatus, error) {
	client, err := membership.NewSerfClient(r.SerfConfig)
	if err != nil {
		return nil, trace.Wrap(err, "failed to connect to serf cluster")
	}

	defer func() {
		if err := client.Close(); err != nil {
			log.WithError(err).Warn("Failed to close serf client")
		}
	}()

	local, err := client.FindMember(r.SerfName)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	status := r.runChecks(ctx)
	status.MemberStatus = statusFromMember(local)
	return status, nil
}

// runChecks executes the monitoring tests configured for this agent in parallel.
func (r *Agent) runChecks(ctx context.Context) *pb.NodeStatus {
	// semaphoreCh limits the number of concurrent checkers.
	semaphoreCh := make(chan struct{}, maxConcurrentCheckers)
	// probeCh collects resulting health probes.
	probeCh := make(chan health.Reporter, len(r.Checkers))

	// Start checkers.
	// If context is done return empty node status.
	for _, checker := range r.Checkers {
		select {
		case semaphoreCh <- struct{}{}:
			go runChecker(ctx, checker, probeCh, semaphoreCh)
		case <-ctx.Done():
			log.WithError(ctx.Err()).Warn("Timed out running tests.")
			return emptyNodeStatus(r.SerfName)
		}
	}

	// Collect probe results.
	// If context is done return degraded status with currently collect probes.
	var reporter *health.Probes
	for i := 0; i < len(r.Checkers); i++ {
		select {
		case probe := <-probeCh:
			health.AddFrom(reporter, probe)
		case <-ctx.Done():
			log.WithError(ctx.Err()).Warn("Timed out collecting test results.")
			return &pb.NodeStatus{
				Name:   r.SerfName,
				Status: pb.NodeStatus_Degraded,
				Probes: reporter.GetProbes(),
			}
		}
	}

	return &pb.NodeStatus{
		Name:   r.SerfName,
		Status: reporter.Status(),
		Probes: reporter.GetProbes(),
	}
}

// runChecker executes the specified checker and reports results on probeCh.
// If the checker panics, the resulting probe will describe the checker failure.
// Semaphore channel is guaranteed to receive a value upon completion.
func runChecker(ctx context.Context, checker health.Checker, probeCh chan<- health.Reporter, semaphoreCh <-chan struct{}) {
	defer func() {
		if err := recover(); err != nil {
			var probes health.Probes
			probes.Add(&pb.Probe{
				Checker:  checker.Name(),
				Status:   pb.Probe_Failed,
				Severity: pb.Probe_Critical,
				Error:    trace.Errorf("checker panicked: %v\n%s", err, debug.Stack()).Error(),
			})
		}
		//release checker slot
		<-semaphoreCh
	}()

	log.Debugf("Running checker %q.", checker.Name())

	// Use a shorter timeout to allow time to report check results.
	ctx, cancel := context.WithTimeout(ctx, checkerTimeout)
	defer cancel()

	checkCh := make(chan health.Reporter, 1)
	go func() {
		// Use a shorter timeout to allow time to cancel the checker before reporting.
		ctx, cancel := context.WithTimeout(ctx, probeTimeout)
		defer cancel()

		var probes *health.Probes
		checker.Check(ctx, probes)
		checkCh <- probes
	}()

	// If Check fails to report probes before parent context is done, return failed
	// probe indicating potential goroutine leak.
	select {
	case probes := <-checkCh:
		probeCh <- probes
	case <-ctx.Done():
		var probes *health.Probes
		probes.Add(&pb.Probe{
			Checker:  checker.Name(),
			Status:   pb.Probe_Failed,
			Severity: pb.Probe_Critical,
			Error:    "checker does not comply with specified context, potential goroutine leak",
		})
		probeCh <- probes
	}
}
