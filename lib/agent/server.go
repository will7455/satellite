package agent

import (
	"context"
	"net/http"
	"strings"
	"time"

	pb "github.com/gravitational/satellite/agent/proto/agentpb"
	"github.com/gravitational/satellite/lib/rpc"
	"github.com/gravitational/satellite/utils"

	"github.com/gravitational/roundtrip"
	"github.com/gravitational/trace"
	log "github.com/sirupsen/logrus"
	"google.golang.org/grpc"
)

type Config struct {
	// RPCAddrs is a list of addresses agent binds to for RPC traffic.
	//
	// Usually, at least two address are used for operation.
	// Localhost is a convenience for local communication.  Cluster-visible
	// IP is required for proper inter-communication between agents.
	RPCAddrs []string
	// RPCConfig specifies rpc configuration.
	RPCConfig rpc.Config
}

// CheckAndSetDefaults validates this configuration object.
// Configvalues that were not specified will be set to their default values if
// available.
func (r *Config) CheckAndSetDefaults() error {
	var errors []error
	if err := (&r.RPCConfig).CheckAndSetDefaults(); err != nil {
		errors = append(errors, err)
	}
	if len(r.RPCAddrs) == 0 {
		errors = append(errors, trace.BadParameter("at least one RPC address must be provided"))
	}
	return trace.NewAggregate(errors...)
}

type MultiplexServer struct {
	Config
	grpcServer   *grpc.Server
	healthServer *HealthServer
}

func NewMultiplexServer() (*MultiplexServer, error) {
	agentService, err := NewAgentService(nil, nil)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	// Init GRPC
	grpcServer, err := rpc.NewGRPCServer(rpc.Config{})
	if err != nil {
		return nil, trace.Wrap(err)
	}
	pb.RegisterAgentServer(grpcServer, agentService)

	// Init HTTP
	healthServer, err := NewHealthServer(agentService)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	return &MultiplexServer{
		grpcServer:   grpcServer,
		healthServer: healthServer,
	}, nil
}

func (r *MultiplexServer) Serve(ctx context.Context) error {
	caCertPool, err := utils.NewCertPool(r.RPCConfig.CAFile)
	if err != nil {
		return trace.Wrap(err)
	}

	rpcServers := make([]*http.Server, 0, len(r.RPCAddrs))
	defer func() {
		for _, server := range rpcServers {
			if err := server.Shutdown(ctx); err != nil {
				log.WithError(err).Errorf("Failed to shutdown rpc server running on %s", server.Addr)
			}
		}
	}()

	errCh := make(chan error, len(r.RPCAddrs))
	for _, addr := range r.RPCAddrs {
		server := &http.Server{
			Addr:      addr,
			TLSConfig: utils.NewTLSConfig(caCertPool),
			Handler:   http.HandlerFunc(r.handler),
		}
		rpcServers = append(rpcServers, server)

		go func(srv *http.Server) {
			err := srv.ListenAndServeTLS(r.RPCConfig.CertFile, r.RPCConfig.KeyFile)
			if err == http.ErrServerClosed {
				log.WithError(err).Debug("Server has been shutdown/closed.")
			}
			errCh <- err
		}(server)
	}

	// Return on first error, or wait for all rpc servers to shutdown.
	select {
	case err := <-errCh:
		return trace.Wrap(err, "a rpc server failed to serve or has unexpectedly shutdown")
	case <-ctx.Done():
		log.Info("Serve context is done.")
		return nil
	}
}

func (r *MultiplexServer) handler(w http.ResponseWriter, req *http.Request) {
	contentType := req.Header.Get("Content-Type")
	if req.ProtoMajor == 2 && strings.Contains(contentType, "application/grpc") {
		r.grpcServer.ServeHTTP(w, req)
		return
	}
	r.healthServer.ServeHTTP(w, req)
}

// HealthServer handles health status requests.
type HealthServer struct {
	agentService *AgentService
}

// NewHealthServer constructs a new HealthServer.
func NewHealthServer(agentService *AgentService) (*HealthServer, error) {
	return &HealthServer{
		agentService: agentService,
	}, nil
}

// ServeHTTP implements the Go standard library's http.Handler
// interface by responding to health status requests.
func (r *HealthServer) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if req.Method != "GET" {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	if req.URL.Path == "/local" || req.URL.Path == "/local/" {
		r.localStatus(w, req)
		return
	}

	r.status(w, req)
}

// status reports the health status of the serf cluster.
func (r *HealthServer) status(w http.ResponseWriter, req *http.Request) {
	status, err := r.agentService.Status(req.Context(), nil)
	if err != nil {
		roundtrip.ReplyJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
		return
	}

	httpStatus := http.StatusOK
	if isDegraded(*status.GetStatus()) {
		httpStatus = http.StatusServiceUnavailable
	}

	roundtrip.ReplyJSON(w, httpStatus, status.GetStatus())
}

// localStatus reports the health status of the local serf node.
func (r *HealthServer) localStatus(w http.ResponseWriter, req *http.Request) {
	status, err := r.agentService.LocalStatus(req.Context(), nil)
	if err != nil {
		roundtrip.ReplyJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
		return
	}

	httpStatus := http.StatusOK
	if isNodeDegraded(*status.GetStatus()) {
		httpStatus = http.StatusServiceUnavailable
	}

	roundtrip.ReplyJSON(w, httpStatus, status.GetStatus())
}

// AgentService handles agent service RPCs.
//
// Implements agentpb.AgentServer
type AgentService struct {
	node   NodeAgent
	master MasterAgent
}

// NewAgentService constructs a new AgentServer.
func NewAgentService(nodeAgent NodeAgent, masterAgent MasterAgent) (*AgentService, error) {
	return &AgentService{
		node:   nodeAgent,
		master: masterAgent,
	}, nil
}

// Status reports the health status of a serf cluster by iterating over the list
// of currently active cluster members and collecting their respective health statuses.
func (r *AgentService) Status(ctx context.Context, _ *pb.StatusRequest) (resp *pb.StatusResponse, err error) {
	status, err := r.node.Status()
	if err != nil {
		return nil, rpc.GRPCError(err)
	}
	return &pb.StatusResponse{Status: status}, nil
}

// LocalStatus reports the health status of the local serf node.
func (r *AgentService) LocalStatus(ctx context.Context, _ *pb.LocalStatusRequest) (resp *pb.LocalStatusResponse, err error) {
	return &pb.LocalStatusResponse{
		Status: r.node.LocalStatus(),
	}, nil
}

// LastSeen returns the last seen timestamp for a specified member.
func (r *AgentService) LastSeen(ctx context.Context, req *pb.LastSeenRequest) (resp *pb.LastSeenResponse, err error) {
	return nil, trace.NotImplemented("not implemented on node")
}

// Time sends back the target node server time
func (r *AgentService) Time(ctx context.Context, _ *pb.TimeRequest) (*pb.TimeResponse, error) {
	return &pb.TimeResponse{
		Timestamp: pb.NewTimeToProto(r.node.Time().UTC()),
	}, nil
}

// Timeline sends the current status timeline
func (r *AgentService) Timeline(ctx context.Context, req *pb.TimelineRequest) (*pb.TimelineResponse, error) {
	return nil, trace.NotImplemented("not implemented on node")
}

// UpdateTimeline updates the timeline with a new event.
// Duplicate requests will have no effect.
func (r *AgentService) UpdateTimeline(ctx context.Context, req *pb.UpdateRequest) (*pb.UpdateResponse, error) {
	return nil, trace.NotImplemented("not implemented on node")
}

type NodeAgent interface {
	Status() (*pb.SystemStatus, error)
	LocalStatus() *pb.NodeStatus
	Time() time.Time
}

type MasterAgent interface{}
