package observation

import (
	"context"
	"fmt"
	api "github.com/qcrs/accelerator-control-plane/api/v1alpha1"
	"github.com/qcrs/accelerator-control-plane/internal/contracts"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"
)

func TestBusyProbeRetriesWithinOneBudget(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		fmt.Fprint(w, `{"schemaVersion":1,"driver":"corex-remote","healthy":true,"capacity":1,"activeSessions":0,"cleanupErrors":0}`)
	}))
	defer server.Close()
	host, port, _ := net.SplitHostPort(server.Listener.Addr().String())
	result, err := Read(context.Background(), host, contracts.RemoteEndpoint{Runtime: api.RuntimeInfo{Driver: "corex-remote"}, Parameters: map[string]string{ParameterPort: port}})
	if err != nil || result.Health != api.DeviceHealthHealthy || calls.Load() != 3 {
		t.Fatalf("busy probe did not recover: result=%+v error=%v calls=%d", result, err, calls.Load())
	}
}

func TestBusyProbeDeadlineNeverBecomesIdle(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()
	host, port, _ := net.SplitHostPort(server.Listener.Addr().String())
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Millisecond)
	defer cancel()
	result, err := Read(ctx, host, contracts.RemoteEndpoint{Parameters: map[string]string{ParameterPort: port}})
	if err == nil || result.Health != api.DeviceHealthUnknown || result.ActiveSessions != nil {
		t.Fatalf("busy timeout was interpreted as free: %+v %v", result, err)
	}
}

func TestObservationBoundary(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		health     api.DeviceHealth
		bad        bool
	}{
		{"idle", `{"schemaVersion":1,"driver":"corex-remote","healthy":true,"capacity":1,"activeSessions":0,"cleanupErrors":0}`, api.DeviceHealthHealthy, false},
		{"active", `{"schemaVersion":1,"driver":"corex-remote","healthy":true,"capacity":1,"activeSessions":2,"cleanupErrors":0}`, api.DeviceHealthHealthy, false},
		{"cleanup failed", `{"schemaVersion":1,"driver":"corex-remote","healthy":true,"capacity":1,"activeSessions":0,"cleanupErrors":1}`, api.DeviceHealthUnhealthy, false},
		{"device unavailable", `{"schemaVersion":1,"driver":"corex-remote","healthy":false,"capacity":0,"activeSessions":0,"cleanupErrors":0}`, api.DeviceHealthUnhealthy, false},
		{"missing sessions", `{"schemaVersion":1,"driver":"corex-remote","healthy":true,"capacity":1,"cleanupErrors":0}`, "", true},
		{"negative", `{"schemaVersion":1,"driver":"corex-remote","healthy":true,"capacity":1,"activeSessions":-1,"cleanupErrors":0}`, "", true},
		{"wrong driver", `{"schemaVersion":1,"driver":"mlu-remote","healthy":true,"capacity":1,"activeSessions":0,"cleanupErrors":0}`, "", true},
		{"malformed", `{`, "", true},
		{"trailing", `{} {}`, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/observation" {
					t.Errorf("unexpected path: %s", r.URL.Path)
				}
				fmt.Fprint(w, tc.body)
			}))
			defer server.Close()
			host, port, _ := net.SplitHostPort(server.Listener.Addr().String())
			result, err := Read(context.Background(), host, contracts.RemoteEndpoint{Runtime: api.RuntimeInfo{Driver: "corex-remote"}, Parameters: map[string]string{ParameterPort: port}})
			if (err != nil) != tc.bad {
				t.Fatalf("result=%+v error=%v", result, err)
			}
			if !tc.bad && (result.Health != tc.health || result.ActiveSessions == nil || result.ObservedAt.IsZero()) {
				t.Fatalf("unexpected result %+v", result)
			}
		})
	}
}

func TestObservationTimeoutAndMissingPortAreUnknown(t *testing.T) {
	result, err := Read(context.Background(), "localhost", contracts.RemoteEndpoint{})
	if err != nil || result.Health != api.DeviceHealthUnknown || result.ActiveSessions != nil {
		t.Fatalf("missing observer became idle: %+v %v", result, err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer server.Close()
	host, port, _ := net.SplitHostPort(server.Listener.Addr().String())
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	result, err = Read(ctx, host, contracts.RemoteEndpoint{Parameters: map[string]string{ParameterPort: port}})
	if err == nil || result.Health != api.DeviceHealthUnknown || result.ActiveSessions != nil {
		t.Fatalf("timeout became idle: %+v %v", result, err)
	}
}

// Enabled only by the independent runtime validation harness. This exercises
// the real provider HTTP decoder against the actual C runtime, not a mock.
func TestLiveRuntimeObservation(t *testing.T) {
	driver := os.Getenv("REMOTE_OBSERVATION_TEST_DRIVER")
	if driver == "" {
		t.Skip("independent runtime harness not running")
	}
	port := os.Getenv("REMOTE_OBSERVATION_TEST_PORT")
	result, err := Read(context.Background(), "localhost", contracts.RemoteEndpoint{Runtime: api.RuntimeInfo{Driver: driver}, Parameters: map[string]string{ParameterPort: port}})
	if err != nil || result.Health != api.DeviceHealthHealthy || result.ActiveSessions == nil || *result.ActiveSessions != 0 || result.AdvertisedCapacity < 1 {
		t.Fatalf("real runtime observation: %+v %v", result, err)
	}
	results := make(chan error, 3)
	for i := 0; i < 3; i++ {
		go func() {
			observation, err := Read(context.Background(), "localhost", contracts.RemoteEndpoint{Runtime: api.RuntimeInfo{Driver: driver}, Parameters: map[string]string{ParameterPort: port}})
			if err == nil && (observation.Health != api.DeviceHealthHealthy || observation.ActiveSessions == nil || *observation.ActiveSessions != 0) {
				err = fmt.Errorf("concurrent observation was not healthy and session-free: %+v", observation)
			}
			results <- err
		}()
	}
	for i := 0; i < 3; i++ {
		if err := <-results; err != nil {
			t.Fatalf("concurrent native probe: %v", err)
		}
	}
}
