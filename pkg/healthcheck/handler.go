// Copyright (c) 2017 Uber Technologies, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package healthcheck

import (
	"context"
	"net"
	"net/http"
	"strconv"
	"sync/atomic"

	"go.uber.org/zap"

	"github.com/jaegertracing/jaeger/pkg/version"
)

// Status represents the state of the service.
type Status int

const (
	// Unavailable indicates the service is not able to handle requests
	Unavailable Status = iota
	// Ready indicates the service is ready to handle requests
	Ready
	// Broken indicates that the healthcheck itself is broken, not serving HTTP
	Broken
	// Fail just indicates a failure
	Fail
)

func (s Status) String() string {
	switch s {
	case Unavailable:
		return "unavailable"
	case Ready:
		return "ready"
	case Broken:
		return "broken"
	default:
		return "unknown"
	}
}

// Component value is designated for all the components under healthcheck.
type Component uint32

const (
	// Default is uninitialized value
	Default = iota
	// Init -ialization phase
	Init
	// Storage availablity
	Storage
	// ArchiveStorage availablity
	ArchiveStorage
	// SamplingStorage is SamplingStrategyStorage
	SamplingStorage
)

func (c Component) String() string {
	switch c {
	case Init:
		return "initialization"
	case Storage:
		return "storage"
	case ArchiveStorage:
		return "archive-storage"
	case SamplingStorage:
		return "sampling-strategy-storage"
	default:
		return "unknown"
	}
}

// ComponentStatus is a message coming from each event of the components
type ComponentStatus struct {
	comp Component
	stat Status
}

// HealthCheck provides an HTTP endpoint that returns the health status of the service
type HealthCheck struct {
	state   int32 // atomic, keep at the top to be word-aligned
	logger  *zap.Logger
	mapping map[Status]int
	server  *http.Server
	comstat map[Component]Status
	desired []Component

	receptor chan ComponentStatus
}

// Option is a functional option for passing parameters to New()
type Option func(*HealthCheck)

// Logger creates an option to set the logger. If not specified, Nop logger is used.
func Logger(logger *zap.Logger) Option {
	return func(hc *HealthCheck) {
		hc.logger = logger
	}
}

// SetDesired is to specify the set of the Components to be up. If all of them are ready, we are ready.
func SetDesired(cs []Component) Option {
	return func(hc *HealthCheck) {
		hc.desired = cs
	}
}

// SetReceptor sets a channel to send ComponentStatus which is created at initialization phase outside of this package.
func SetReceptor(rec chan ComponentStatus) Option {
	return func(hc *HealthCheck) {
		hc.receptor = rec
	}
}

// New creates a HealthCheck with the specified initial state.
func New(state Status, options ...Option) *HealthCheck {
	hc := &HealthCheck{
		state: int32(state),
		mapping: map[Status]int{
			Unavailable: http.StatusServiceUnavailable,
			Ready:       http.StatusNoContent,
		},
	}
	for _, option := range options {
		option(hc)
	}
	if hc.logger == nil {
		hc.logger = zap.NewNop()
	}
	hc.comstat = make(map[Component]Status, len(hc.desired))
	go hc.monitor()
	return hc
}

// Serve starts HTTP server on the specified port.
func (hc *HealthCheck) Serve(port int) (*HealthCheck, error) {
	portStr := ":" + strconv.Itoa(port)
	l, err := net.Listen("tcp", portStr)
	if err != nil {
		hc.logger.Error("Health Check server failed to listen", zap.Error(err))
		return nil, err
	}
	hc.serveWithListener(l)
	hc.logger.Info("Health Check server started", zap.Int("http-port", port), zap.Stringer("status", hc.Get()))
	return hc, nil
}

func (hc *HealthCheck) serveWithListener(l net.Listener) {
	hc.server = &http.Server{Handler: hc.httpHandler()}
	go func() {
		if err := hc.server.Serve(l); err != nil {
			hc.logger.Error("failed to serve", zap.Error(err))
			hc.Set(Broken)
		}
	}()
}

// Close stops the HTTP server
func (hc *HealthCheck) Close() error {
	return hc.server.Shutdown(context.Background())
}

// httpHandler creates a new HTTP handler.
func (hc *HealthCheck) httpHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(hc.mapping[hc.Get()])
		// this is written only for response with an entity, so, it won't be used for a 204 - No content
		w.Write([]byte("Server not available"))
	})
	version.RegisterHandler(mux, hc.logger)
	return mux
}

// Set a new health check status
func (hc *HealthCheck) Set(state Status) {
	atomic.StoreInt32(&hc.state, int32(state))
	hc.logger.Info("Health Check state change", zap.Stringer("status", hc.Get()))
}

// Get the current status of this health check
func (hc *HealthCheck) Get() Status {
	return Status(atomic.LoadInt32(&hc.state))
}

// Ready is a shortcut for Set(Ready) (kept for backwards compatibility)
func (hc *HealthCheck) Ready() {
	hc.Set(Ready)
}

// Monitor the receptor's report coming from components on ready or unready
func (hc *HealthCheck) monitor() {
	for msg := range hc.receptor {
		hc.logger.Info("Component health state change", zap.Stringer("component", msg.comp), zap.Stringer("state", msg.stat))
		hc.comstat[msg.comp] = msg.stat
		hc.checkComponent()
	}
}

// All the goodies there? Let's go!
func (hc *HealthCheck) checkComponent() {
	ok := true
	for _, c := range hc.desired {
		if hc.comstat[c] != Ready {
			ok = false
			break
		}
	}
	if ok {
		hc.Set(Ready)
	} else {
		hc.Set(Unavailable)
	}
}

// StatusReporter is called on an availablity change event.
type StatusReporter func(Status)

// GetNullStatusReporter returns the mock StatusReporter.
func GetNullStatusReporter() func(Status) {
	return func(_ Status) {}
}

// GetStatusReporter is a vending machine of gifts for the components. Each component talks to their own teddy bear and he reports their confess for us.
func (hc *HealthCheck) GetStatusReporter(c Component) func(Status) {
	return func(stat Status) {
        if hc.receptor == nil {
            hc.logger.Warn("No channel for component status report")
            return
        }
		hc.receptor <- ComponentStatus{
			stat: stat,
			comp: c,
		}
	}
}
