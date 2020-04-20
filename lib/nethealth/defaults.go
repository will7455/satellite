/*
Copyright 2019 Gravitational, Inc.

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

// Package nethealth implements a daemonset that when deployed to a kubernetes cluster, will locate and send ICMP echos
// (pings) to the nethealth pod on every other node in the cluster. This will give an indication into whether the
// overlay network is functional for pod -> pod communications, and also record packet loss on the network.
package nethealth

import (
	"time"
)

const (
	// heartbeatInterval is the duration between sending heartbeats to each peer. Any heartbeat that takes more
	// than one interval to respond will also be considered timed out.
	heartbeatInterval = 1 * time.Second

	// resyncInterval is the duration between full resyncs of local state with kubernetes. If a node is deleted it
	// may not be detected until the full resync completes.
	resyncInterval = 15 * time.Minute

	// dnsDiscoveryInterval is the duration of time for doing DNS based service discovery for pod changes. This is a
	// lightweight test for whether there is a change to the nethealth pods within the cluster.
	dnsDiscoveryInterval = 10 * time.Second

	// Default selector to use for finding nethealth pods
	DefaultSelector = "k8s-app=nethealth"

	// DefaultServiceDiscoveryQuery is the default name to query for service discovery changes
	DefaultServiceDiscoveryQuery = "any.nethealth"

	// DefaultPrometheusPort is the default port to serve prometheus metrics
	DefaultPrometheusPort = 9801

	// DefaultNamespace is the default namespace to search for nethealth resources
	DefaultNamespace = "monitoring"
)

const (
	// Init is peer state that we've found the node but don't know anything about it yet.
	Init = "init"
	// Up is a peer state that the peer is currently reachable
	Up = "up"
	// Timeout is a peer state that the peer is currently timing out to pings
	Timeout = "timeout"
)
