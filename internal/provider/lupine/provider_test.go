package lupine

import (
	"fmt"
	"strings"
	"testing"
)

func TestServerListPreservesOrder(t *testing.T) {
	got, err := ServerList([]string{"https://gpu-a.example", "gpu-b:14833", "http://gpu-c"})
	if err != nil {
		t.Fatalf("ServerList returned error: %v", err)
	}
	want := "https://gpu-a.example,gpu-b:14833,http://gpu-c"
	if got != want {
		t.Fatalf("expected %q, got %q", want, got)
	}
}

func TestServerListRejectsUnsafeOrUnsupportedEndpoints(t *testing.T) {
	tests := []string{
		"https://user:password@gpu.example",
		"ftp://gpu.example",
		"gpu-a:14833,gpu-b:14833",
		"https://gpu.example/path",
		"gpu.example?token=value",
		"gpu.example#fragment",
	}
	for _, endpoint := range tests {
		t.Run(endpoint, func(t *testing.T) {
			if _, err := ServerList([]string{endpoint}); err == nil {
				t.Fatalf("expected %q to be rejected", endpoint)
			}
		})
	}
}

func TestServerListEnforcesLupineConnectionLimit(t *testing.T) {
	endpoints := make([]string, maximumServerEntries+1)
	for index := range endpoints {
		endpoints[index] = fmt.Sprintf("gpu-%d:14833", index)
	}
	_, err := ServerList(endpoints)
	if err == nil || !strings.Contains(err.Error(), "at most") {
		t.Fatalf("expected connection limit error, got %v", err)
	}
}
