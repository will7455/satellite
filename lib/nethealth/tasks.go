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
	"fmt"
	"net"
	"net/http"
	"reflect"
	"sort"
	"time"

	"github.com/gravitational/trace"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/sirupsen/logrus"
	"golang.org/x/net/icmp"
)

// loopMain is the main processsing loop for sending/receiving heartbeats and
// modifying application state.
func loopMain(ctx context.Context) {
	app := ctx.Value("app").(*Application)
	app.Info("Starting main application loop.")

	for {
		select {
		case <-ctx.Done():
			app.Info("Stopping main application loop.")
			return
		case <-app.triggerResync:
			if err := app.resync(); err != nil {
				app.WithError(err).Error("Unexpected error re-syncing the nethealth nodes.")
			}
		case <-app.triggerHeartbeat:
			app.checkTimeouts()
			app.sendHeartbeats()
		case rx := <-app.rxMessage:
			if err := app.processAck(rx); err != nil {
				app.WithFields(logrus.Fields{
					logrus.ErrorKey: err,
					"pod_addr":      rx.podAddr,
					"rx_time":       rx.rxTime,
					"message":       rx.message,
				}).Error("Error processing icmp message.")
			}
		}
	}
}

func loopHeartbeat(ctx context.Context) {
	app := ctx.Value("app").(*Application)
	app.Info("Starting heartbeat loop.")

	ticker := app.NewTicker(heartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			app.Info("Stopping heartbeat loop.")
			return
		case <-ticker.Chan():
			select {
			case app.triggerHeartbeat <- struct{}{}:
			default: // Don't block
			}
		}
	}
}

func loopResync(ctx context.Context) {
	app := ctx.Value("app").(*Application)
	app.Info("Starting nethealth resync loop.")

	ticker := app.NewTicker(resyncInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			app.Info("Stopping nethealth resync loop")
			return
		case <-ticker.Chan():
			select {
			case app.triggerResync <- struct{}{}:
			default: // Don't block
			}
		}
	}
}

func loopServiceDiscovery(ctx context.Context) {
	app := ctx.Value("app").(*Application)
	app.Info("Starting DNS service discovery for nethealth pod.")

	var previousNames []string

	ticker := app.NewTicker(dnsDiscoveryInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			app.Info("Stopping DNS service discovery.")
			return
		case <-ticker.Chan():
			if err := serviceDiscovery(ctx, previousNames); err != nil {
				app.WithError(err).Debug("Failed to complete service discovery.")
			}
		}
	}
}

func serviceDiscovery(ctx context.Context, previousNames []string) error {
	app := ctx.Value("app").(*Application)

	app.Debugf("Querying %v for service discovery", app.ServiceDiscoveryQuery)
	names, err := net.LookupHost(app.ServiceDiscoveryQuery)
	if err != nil {
		return trace.Wrap(err, "failed to query service discovery").AddField("query", app.ServiceDiscoveryQuery)
	}

	sort.Strings(names)
	if reflect.DeepEqual(names, previousNames) {
		return nil
	}

	previousNames = names
	app.Info("Triggering peer resync due to service discovery change")

	select {
	case app.triggerResync <- struct{}{}:
	default: // Don't block
	}

	return nil
}

func loopHandleICMP(ctx context.Context) {
	app := ctx.Value("app").(*Application)
	app.Info("Starting icmp message handler.")

	buf := make([]byte, 256)
	for {
		select {
		case <-ctx.Done():
			app.Info("Stopping icmp handler.")
			return
		default:
			if err := handleICMP(ctx, buf); err != nil {
				app.WithError(err).Error("Error handling icmp message.")
			}
		}
	}
}

func handleICMP(ctx context.Context, buf []byte) error {
	app := ctx.Value("app").(*Application)
	app.Info("Starting icmp message handler.")

	n, podAddr, err := app.conn.ReadFrom(buf)
	if err != nil {
		return trace.Wrap(err, "error in udp socket read.")
	}

	// The ICMP package doesn't export the protocol numbers
	// 1 - ICMP
	// 58 - ICMPv6
	// https://www.iana.org/assignments/protocol-numbers/protocol-numbers.xhtml
	msg, err := icmp.ParseMessage(1, buf[:n])
	if err != nil {
		return trace.Wrap(err, "Failed to parse icmp message.").AddFields(trace.Fields{
			"podAddr": podAddr,
			"msg":     msg,
		})
	}

	select {
	case app.rxMessage <- messageWrapper{
		message: msg,
		rxTime:  app.Clock.Now(),
		podAddr: podAddr,
	}:
	default: // Don't block
		app.Warn("Dropped icmp message due to full rxMessage queue")
	}
	return nil
}

func serveMetrics(ctx context.Context) error {
	app := ctx.Value("app").(*Application)

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	mux := http.ServeMux{}
	mux.Handle("/metrics", promhttp.Handler())
	httpServer := &http.Server{Addr: fmt.Sprintf(":%d", app.PrometheusPort), Handler: &mux}

	errChan := make(chan error, 1)
	go func() {
		app.Info("Serving nethealth metrics on port %s", app.PrometheusPort)
		errChan <- httpServer.ListenAndServe()
	}()

	select {
	case err := <-errChan:
		if err != http.ErrServerClosed {
			return trace.Wrap(err)
		}
	case <-ctx.Done():
		const shutdownTimeout = 5 * time.Second
		ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		if err := httpServer.Shutdown(ctx); err != nil {
			return trace.Wrap(err)
		}
	}

	return nil
}
