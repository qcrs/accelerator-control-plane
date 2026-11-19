// Package observation reads the bounded, read-only management endpoint exposed
// by remote runtimes. Probing never creates a GPU/NPU session.
package observation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"time"

	api "github.com/qcrs/accelerator-control-plane/api/v1alpha1"
	"github.com/qcrs/accelerator-control-plane/internal/contracts"
)

const ParameterPort = "observationPort"

// Snapshot is v1 of the runtime observation contract. Counts include sessions
// awaiting handshake or cleanup; a cleanup failure latches the server degraded.
type Snapshot struct {
	SchemaVersion  int    `json:"schemaVersion"`
	Driver         string `json:"driver"`
	Healthy        *bool  `json:"healthy"`
	Capacity       *int32 `json:"capacity"`
	ActiveSessions *int32 `json:"activeSessions"`
	CleanupErrors  *int32 `json:"cleanupErrors"`
}

func Read(ctx context.Context, host string, endpoint contracts.RemoteEndpoint) (contracts.EndpointObservation, error) {
	result := contracts.EndpointObservation{Health: api.DeviceHealthUnknown, Source: "remote-probe", Confidence: "Unknown", Reason: "ObservationUnavailable"}
	port := endpoint.Parameters[ParameterPort]
	if port == "" {
		result.Reason = "ObservationNotConfigured"
		result.Message = "server observationPort is not configured; remote sessions are unknown"
		return result, nil
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return result, fmt.Errorf("invalid observationPort %q", port)
	}
	// Never use an HTTP proxy or a redirect to query a different runtime.
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	transport := &http.Transport{Proxy: nil, DisableKeepAlives: true}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 2 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	started := time.Now().UTC()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+net.JoinHostPort(host, port)+"/v1/observation", nil)
	if err != nil {
		return result, err
	}
	var response *http.Response
	delay := 10 * time.Millisecond
	for {
		response, err = client.Do(req)
		busy := err == nil && response.StatusCode == http.StatusServiceUnavailable
		if !busy && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
			break
		}
		if response != nil {
			response.Body.Close()
		}
		// A native probe is single-flight. Retry only its explicit busy
		// response and legacy premature EOF, inside one shared time budget.
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return result, fmt.Errorf("observe runtime: %w", ctx.Err())
		case <-timer.C:
		}
		if delay < 100*time.Millisecond {
			delay *= 2
			if delay > 100*time.Millisecond {
				delay = 100 * time.Millisecond
			}
		}
	}
	if err != nil {
		return result, fmt.Errorf("observe runtime: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return result, fmt.Errorf("observation HTTP status %d", response.StatusCode)
	}
	var snapshot Snapshot
	decoder := json.NewDecoder(io.LimitReader(response.Body, 4097))
	if err := decoder.Decode(&snapshot); err != nil {
		return result, fmt.Errorf("decode observation: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return result, fmt.Errorf("trailing observation data")
	}
	if snapshot.SchemaVersion != 1 || snapshot.Driver != endpoint.Runtime.Driver || snapshot.Healthy == nil || snapshot.Capacity == nil || snapshot.ActiveSessions == nil || snapshot.CleanupErrors == nil || *snapshot.Capacity < 0 || *snapshot.ActiveSessions < 0 || *snapshot.CleanupErrors < 0 {
		return result, fmt.Errorf("invalid or incomplete observation for driver %q", endpoint.Runtime.Driver)
	}
	result.ObservedAt = started
	result.AdvertisedCapacity = *snapshot.Capacity
	result.ActiveSessions = snapshot.ActiveSessions
	result.Confidence = "Observed"
	result.Health = api.DeviceHealthHealthy
	result.Reason = "RuntimeObserved"
	result.Message = "runtime responded with device enumeration and live session/cleanup accounting; this is not a kernel correctness test"
	if !*snapshot.Healthy || *snapshot.Capacity == 0 || *snapshot.CleanupErrors > 0 {
		result.Health = api.DeviceHealthUnhealthy
		result.Reason = "RuntimeUnhealthy"
		result.Message = fmt.Sprintf("runtime healthy=%t capacity=%d cleanupErrors=%d", *snapshot.Healthy, *snapshot.Capacity, *snapshot.CleanupErrors)
	}
	return result, nil
}
