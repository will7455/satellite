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

package nethealth

import (
	"context"
	"net"
	"time"

	"github.com/gravitational/satellite/utils"
	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"

	"github.com/gravitational/trace"
	"github.com/jonboulle/clockwork"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

type AppConfig struct {
	// Clock will be used for internal time keeping.
	clockwork.Clock
	// PrometheusPort is the port to bind to for serving prometheus metrics
	PrometheusPort uint32
	// Namespace is the kubernetes namespace to monitor for other nethealth instances
	Namespace string
	// HostIP is the host IP address
	HostIP string
	// Selector is a kubernetes selector to find all the nethealth pods in the configured namespace
	Selector string
	// ServiceDiscoveryQuery is a DNS name that will be used for lightweight service discovery checks. A query to
	// any.<service>.default.svc.cluster.local will return a list of pods for the service. If the list of pods
	// changes we know to resync with the kubernetes API. This method uses significantly less resources than running a
	// kubernetes watcher on the API. Defaults to any.nethealth which will utilize the search path from resolv.conf.
	ServiceDiscoveryQuery string
}

// checkAndSetDefaults validates that this configuration is correct and sets
// value defaults where necessary.
func (r *AppConfig) checkAndSetDefaults() error {
	var errors []error
	if r.HostIP == "" {
		errors = append(errors, trace.BadParameter("host ip must be provided"))
	}
	if r.PrometheusPort == 0 {
		r.PrometheusPort = DefaultPrometheusPort
	}
	if r.Namespace == "" {
		r.Namespace = DefaultNamespace
	}
	if r.Selector == "" {
		r.Selector = DefaultSelector
	}
	if r.ServiceDiscoveryQuery == "" {
		r.ServiceDiscoveryQuery = DefaultServiceDiscoveryQuery
	}
	if r.Clock == nil {
		r.Clock = clockwork.NewRealClock()
	}
	return trace.NewAggregate(errors...)
}

// Application is an instance of nethealth that is running on each node.
type Application struct {
	AppConfig
	logrus.FieldLogger

	// conn specifies icmp packet network endpoint used to send and receive icmp messages
	conn *icmp.PacketConn

	// selector is used to query nethealth pods.
	selector labels.Selector

	// client is used to interface with the the kubernetes API.
	client kubernetes.Interface

	// rxMessage is a processing queue of received echo responses
	rxMessage chan messageWrapper

	// triggerHeartbeat triggers heartbeat.
	triggerHeartbeat chan struct{}

	// triggerResync triggers resync of nethealth pods.
	triggerResync chan struct{}

	// peers maps the ip address of a node to it's peer data.
	peers map[string]*peer

	// podToHost maps the nethealth pod IP to the node host IP.
	podToHost map[string]string

	// Registered Prometheus metrics.
	promPeerRTT     *prometheus.HistogramVec
	promPeerTimeout *prometheus.CounterVec
	promPeerRequest *prometheus.CounterVec
}

func NewApp(config AppConfig) (*Application, error) {
	if err := config.checkAndSetDefaults(); err != nil {
		return nil, trace.Wrap(err)
	}

	// Initialize nethealth label selector
	labelSelector, err := labels.Parse(config.Selector)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	// Initialize kubernetes clientset
	kubernetesConfig, err := rest.InClusterConfig()
	if err != nil {
		return nil, trace.Wrap(err)
	}
	client, err := kubernetes.NewForConfig(kubernetesConfig)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	app := &Application{
		AppConfig:        config,
		FieldLogger:      logrus.WithField(trace.Component, "nethealth"),
		selector:         labelSelector,
		client:           client,
		triggerResync:    make(chan struct{}, 1),
		triggerHeartbeat: make(chan struct{}, 1),
		rxMessage:        make(chan messageWrapper, 100),
		peers:            make(map[string]*peer),
		podToHost:        make(map[string]string),
	}
	app.registerMetrics()
	return app, nil
}

func (r *Application) registerMetrics() {
	r.promPeerRTT = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "nethealth",
		Subsystem: "echo",
		Name:      "duration_seconds",
		Help:      "The round trip time to reach the peer",
		Buckets: []float64{
			0.0001, // 0.1 ms
			0.0002, // 0.2 ms
			0.0003, // 0.3 ms
			0.0004, // 0.4 ms
			0.0005, // 0.5 ms
			0.0006, // 0.6 ms
			0.0007, // 0.7 ms
			0.0008, // 0.8 ms
			0.0009, // 0.9 ms
			0.001,  // 1ms
			0.0015, // 1.5ms
			0.002,  // 2ms
			0.003,  // 3ms
			0.004,  // 4ms
			0.005,  // 5ms
			0.01,   // 10ms
			0.02,   // 20ms
			0.04,   // 40ms
			0.08,   // 80ms
		},
	}, []string{"node_name", "peer_name"})
	r.promPeerTimeout = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "nethealth",
		Subsystem: "echo",
		Name:      "timeout_total",
		Help:      "The number of echo requests that have timed out",
	}, []string{"node_name", "peer_name"})
	r.promPeerRequest = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "nethealth",
		Subsystem: "echo",
		Name:      "request_total",
		Help:      "The number of echo requests that have been sent",
	}, []string{"node_name", "peer_name"})

	prometheus.MustRegister(
		r.promPeerRTT,
		r.promPeerTimeout,
		r.promPeerRequest,
	)
}

func (r *Application) Run(ctx context.Context) error {
	ctx = context.WithValue(ctx, "application", r)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	errChan := make(chan error, 10)
	go loopMain(ctx)
	go loopServiceDiscovery(ctx)
	go loopHeartbeat(ctx)
	go loopResync(ctx)
	go loopHandleICMP(ctx)
	go func() {
		if err := serveMetrics(ctx); err != nil {
			errChan <- err
		}
	}()

	select {
	case <-ctx.Done():
		r.Info("Nethealth shutting down.")
	case err := <-errChan:
		return trace.Wrap(err)
	}

	return nil
}

func (r *Application) resync() error {
	pods, err := r.client.CoreV1().Pods(r.Namespace).List(metav1.ListOptions{
		LabelSelector: r.selector.String(),
	})
	if err != nil {
		return utils.ConvertError(err)
	}
	r.resyncNethealth(pods.Items)
	return nil
}

func (r *Application) resyncNethealth(pods []corev1.Pod) {
	// keep track of current peers so we can prune nodes that are no longer part of the cluster
	peerMap := make(map[string]bool)
	for _, pod := range pods {
		// Don't add our own node as a peer
		if pod.Status.HostIP == r.HostIP {
			continue
		}

		peerMap[pod.Status.HostIP] = true
		if peer, ok := r.peers[pod.Status.HostIP]; ok {
			peer.updatePodAddr(pod.Status.PodIP) // incase pod IP has changed
			r.podToHost[pod.Status.PodIP] = pod.Status.HostIP
			continue
		}

		// Initialize new peers
		r.peers[pod.Status.HostIP] = newPeer(pod.Status.HostIP, pod.Status.PodIP, r.Clock.Now())
		r.podToHost[pod.Status.PodIP] = pod.Status.HostIP
		r.WithField("peer", pod.Status.HostIP).Info("Adding peer.")

		// Initialize the peer so it shows up in prometheus with a 0 count
		r.promPeerTimeout.WithLabelValues(r.HostIP, pod.Status.HostIP).Add(0)
		r.promPeerRequest.WithLabelValues(r.HostIP, pod.Status.HostIP).Add(0)
	}

	// check for peers that have been deleted
	for _, peer := range r.peers {
		if _, ok := peerMap[peer.hostIP]; !ok {
			r.WithField("peer", peer.hostIP).Info("Deleting peer.")
			delete(r.peers, peer.hostIP)
			delete(r.podToHost, peer.podAddr.String())

			r.promPeerRTT.DeleteLabelValues(r.HostIP, peer.hostIP)
			r.promPeerRequest.DeleteLabelValues(r.HostIP, peer.hostIP)
			r.promPeerTimeout.DeleteLabelValues(r.HostIP, peer.hostIP)
		}
	}
}

func (r *Application) sendHeartbeats() {
	for _, peer := range r.peers {
		log := r.WithFields(logrus.Fields{
			"host_ip":  peer.hostIP,
			"pod_addr": peer.podAddr,
			"id":       peer.echoCounter,
		})
		if err := r.sendHeartbeat(peer); err != nil {
			log.WithError(err).Warn("Failed to send heartbeat.")
			continue
		}
		log.Debug("Sent echo request.")
	}
}

func (r *Application) sendHeartbeat(peer *peer) error {
	peer.echoCounter++

	if peer.podAddr == nil || peer.podAddr.String() == "" || peer.podAddr.String() == "0.0.0.0" {
		return trace.BadParameter("invalid pod address: %s", peer.podAddr.String())
	}

	msg := newEcho(peer.echoCounter)
	buf, err := msg.Marshal(nil)
	if err != nil {
		return trace.Wrap(err, "failed to marshal ping")
	}

	peer.echoTime = r.Clock.Now()
	_, err = r.conn.WriteTo(buf, peer.podAddr)
	if err != nil {
		return trace.Wrap(err, "failed to send ping")
	}

	r.promPeerRequest.WithLabelValues(r.HostIP, peer.hostIP).Inc()
	peer.echoTimeout = true

	return nil
}

// checkTimeouts iterates over each peer and checks whether our last heartbeat has timed out.
func (r *Application) checkTimeouts() {
	for _, peer := range r.peers {
		// if the echoTimeout flag is set, it means we didn't receive a response to our last request
		if peer.echoTimeout {
			r.WithFields(logrus.Fields{
				"host_ip":  peer.hostIP,
				"pod_addr": peer.podAddr,
				"id":       peer.echoCounter,
			}).Debug("echo timeout")
			r.promPeerTimeout.WithLabelValues(r.HostIP, peer.hostIP).Inc()
			peer.updateStatus(Timeout, r.Clock.Now())
		}
	}
}

// processAck processes a received ICMP Ack message.
func (r *Application) processAck(e messageWrapper) error {
	switch e.message.Type {
	case ipv4.ICMPTypeEchoReply: // ok
		if err := r.processEchoReply(e.message.Body, e.podAddr, e.rxTime); err != nil {
			return trace.Wrap(err, "failed to process echo reply")
		}
	case ipv4.ICMPTypeEcho: // nothing with echo requests
		return nil
	default: // unexpected / unknown
		return trace.BadParameter("received unexpected icmp message type").AddField("type", e.message.Type)
	}
	return nil
}

func (r *Application) processEchoReply(body icmp.MessageBody, podAddr net.Addr, rxTime time.Time) error {
	switch pkt := body.(type) {
	case *icmp.Echo:
		peer, err := r.lookupPeer(podAddr.String())
		if err != nil {
			return trace.BadParameter("received echo reply from unknown pod %s", podAddr)
		}

		if uint16(pkt.Seq) != uint16(peer.echoCounter) {
			return trace.BadParameter("response sequence doesn't match latest request.").
				AddField("expected", uint16(peer.echoCounter)).
				AddField("received", uint16(pkt.Seq))
		}

		rtt := rxTime.Sub(peer.echoTime)
		r.promPeerRTT.WithLabelValues(r.HostIP, peer.hostIP).Observe(rtt.Seconds())
		peer.updateStatus(Up, r.Clock.Now())
		peer.echoTimeout = false

		r.WithFields(logrus.Fields{
			"host_ip":  peer.hostIP,
			"pod_addr": peer.podAddr,
			"counter":  peer.echoCounter,
			"seq":      uint16(peer.echoCounter),
			"rtt":      rtt,
		}).Debug("Ack.")
	default:
		r.WithField("pod_addr", podAddr.String()).Warn("Unexpected icmp message")
	}
	return nil
}

func (r *Application) lookupPeer(podIP string) (*peer, error) {
	hostIP, ok := r.podToHost[podIP]
	if !ok {
		return nil, trace.BadParameter("pod IP not found in lookup table").AddField("pod_addr", podIP)
	}

	p, ok := r.peers[hostIP]
	if !ok {
		return nil, trace.BadParameter("peer not found in peer table").AddField("host_ip", hostIP)
	}
	return p, nil
}

func newEcho(seq int) icmp.Message {
	return icmp.Message{
		Type: ipv4.ICMPTypeEcho,
		Code: 0,
		Body: &icmp.Echo{
			ID:  1,
			Seq: seq,
		},
	}
}

type messageWrapper struct {
	message *icmp.Message
	rxTime  time.Time
	podAddr net.Addr
}

type peer struct {
	// hostIP specifies the peer's node IP.
	hostIP string
	// podAddr specifies the nethealth pod's IP address.
	podAddr net.Addr

	echoCounter int
	echoTime    time.Time
	echoTimeout bool

	// status specifies the current peer's status.
	status string
	// lastStatusChange specifies the timestamp when the peer's status was last changed.
	lastStatusChange time.Time
}

// newPeer constructs a new peer with the provided config values.
func newPeer(hostIP, podIP string, ts time.Time) *peer {
	return &peer{
		hostIP:           hostIP,
		lastStatusChange: ts,
		podAddr:          &net.IPAddr{IP: net.ParseIP(podIP)},
	}
}

// updatePodAddr updates the peer's pod address.
func (r *peer) updatePodAddr(podIP string) {
	newAddr := &net.IPAddr{
		IP: net.ParseIP(podIP),
	}
	if r.podAddr.String() == newAddr.String() {
		return
	}
	logrus.WithFields(logrus.Fields{
		"host_ip":       r.hostIP,
		"new_peer_addr": newAddr,
		"old_peer_addr": r.podAddr,
	}).Info("Updating peer pod IP address.")
	r.podAddr = newAddr
}

// updateStatus updates the peer's status.
func (r *peer) updateStatus(status string, ts time.Time) {
	if r.status == status {
		return
	}
	logrus.WithFields(logrus.Fields{
		"host_ip":    r.hostIP,
		"pod_addr":   r.podAddr,
		"duration":   ts.Sub(r.lastStatusChange),
		"old_status": r.status,
		"new_status": status,
	}).Info("Peer status changed.")
	r.status = status
	r.lastStatusChange = ts
}
