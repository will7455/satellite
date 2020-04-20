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
package nethealth

import (
	"net"
	"testing"
	"time"

	"github.com/gravitational/trace"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"

	"github.com/jonboulle/clockwork"
	v1 "k8s.io/api/core/v1"
)

const ns = "test-namespace"
const testHostIP = "172.28.128.101"
const testPodIP = "10.128.0.1"

func TestResyncPeerList(t *testing.T) {
	app := testApp()
	cases := []struct {
		pods           []v1.Pod
		expectedPeers  map[string]*peer
		expectedLookup map[string]string
		description    string
	}{
		{
			description: "skip our own server",
			pods: []v1.Pod{
				newTestPod(testHostIP, testPodIP),
			},
			expectedPeers:  map[string]*peer{},
			expectedLookup: map[string]string{},
		},
		{
			description: "add a peer to the cluster",
			pods: []v1.Pod{
				newTestPod("172.28.128.102", "10.128.0.2"),
			},
			expectedPeers: map[string]*peer{
				"172.28.128.102": newTestPeer("172.28.128.102", "10.128.0.2", app.Clock.Now()),
			},
			expectedLookup: map[string]string{
				"10.128.0.2": "172.28.128.102",
			},
		},
		{
			description: "add multiple peers",
			pods: []v1.Pod{
				newTestPod("172.28.128.102", "10.128.0.2"),
				newTestPod("172.28.128.103", "10.128.0.3"),
				newTestPod("172.28.128.104", "10.128.0.4"),
			},
			expectedPeers: map[string]*peer{
				"172.28.128.102": newTestPeer("172.28.128.102", "10.128.0.2", app.Clock.Now()),
				"172.28.128.103": newTestPeer("172.28.128.103", "10.128.0.3", app.Clock.Now()),
				"172.28.128.104": newTestPeer("172.28.128.104", "10.128.0.4", app.Clock.Now()),
			},
			expectedLookup: map[string]string{
				"10.128.0.2": "172.28.128.102",
				"10.128.0.3": "172.28.128.103",
				"10.128.0.4": "172.28.128.104",
			},
		},
	}

	for _, tt := range cases {
		app.resyncNethealth(tt.pods)
		assert.Equal(t, tt.expectedPeers, app.peers, tt.description)
		assert.Equal(t, tt.expectedLookup, app.podToHost, tt.description)
	}
}

func testApp() *Application {
	config := AppConfig{
		Clock:     clockwork.NewFakeClock(),
		Namespace: ns,
		HostIP:    "172.28.128.101",
	}
	app := &Application{
		AppConfig:   config,
		FieldLogger: logrus.WithField(trace.Component, "nethealth"),
		peers:       make(map[string]*peer),
		podToHost:   make(map[string]string),
	}
	app.registerMetrics()
	return app
}

// newTestPod constructs a new pods with the provided hostIP and podIP
func newTestPod(hostIP, podIP string) v1.Pod {
	return v1.Pod{
		Status: v1.PodStatus{
			HostIP: hostIP,
			PodIP:  podIP,
		},
	}
}

// newTestPeer constructs a new peer with the provided config values.
func newTestPeer(hostIP, podAddr string, ts time.Time) *peer {
	return &peer{
		hostIP:           hostIP,
		podAddr:          &net.IPAddr{IP: net.ParseIP(podAddr)},
		lastStatusChange: ts,
	}
}
