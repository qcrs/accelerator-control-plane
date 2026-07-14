package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	acceleratorv1alpha1 "github.com/qcrs/accelerator-control-plane/api/v1alpha1"
	"github.com/qcrs/accelerator-control-plane/internal/agent"
	coordinationv1 "k8s.io/api/coordination/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
)

func main() {
	config := agent.Config{}
	flag.StringVar(&config.EndpointName, "endpoint-name", "", "AcceleratorEndpoint name")
	flag.StringVar(&config.PoolName, "pool-name", "", "AcceleratorPool name")
	flag.StringVar(&config.AgentID, "agent-id", "", "Stable Agent identity")
	flag.StringVar(&config.ControlAddress, "control-address", "", "Advertised Agent control address")
	flag.StringVar(&config.SystemNamespace, "system-namespace", "accelerator-system", "Heartbeat Lease namespace")
	flag.DurationVar(&config.HeartbeatInterval, "heartbeat-interval", 15*time.Second, "Heartbeat renewal interval")
	flag.DurationVar(&config.LeaseDuration, "lease-duration", 45*time.Second, "Heartbeat Lease duration")
	zapOptions := zap.Options{Development: true}
	zapOptions.BindFlags(flag.CommandLine)
	flag.Parse()
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&zapOptions)))

	if err := config.Validate(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(coordinationv1.AddToScheme(scheme))
	utilruntime.Must(acceleratorv1alpha1.AddToScheme(scheme))
	kubernetesClient, err := client.New(ctrl.GetConfigOrDie(), client.Options{Scheme: scheme})
	if err != nil {
		ctrl.Log.Error(err, "create Kubernetes client")
		os.Exit(1)
	}

	// The transport-neutral service is wired now; a future HTTP/gRPC adapter
	// can expose it without changing allocation semantics.
	_ = &agent.Service{Provisioner: agent.UnsupportedProvisioner{}}
	runner := &agent.Runner{
		Client: kubernetesClient,
		Probe:  agent.UnsupportedProbe{},
		Config: config,
		OnError: func(err error) {
			ctrl.Log.Error(err, "reconcile Remote Agent state")
		},
	}
	if err := runner.Run(ctrl.SetupSignalHandler()); err != nil {
		ctrl.Log.Error(err, "run Remote Agent")
		os.Exit(1)
	}
}
