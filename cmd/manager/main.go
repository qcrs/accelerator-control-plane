package main

import (
	"flag"
	"os"

	acceleratorv1alpha1 "github.com/qcrs/accelerator-control-plane/api/v1alpha1"
	"github.com/qcrs/accelerator-control-plane/internal/allocation"
	"github.com/qcrs/accelerator-control-plane/internal/contracts"
	workloadcontroller "github.com/qcrs/accelerator-control-plane/internal/controller"
	"github.com/qcrs/accelerator-control-plane/internal/injector"
	"github.com/qcrs/accelerator-control-plane/internal/inventory"
	"github.com/qcrs/accelerator-control-plane/internal/policy"
	"github.com/qcrs/accelerator-control-plane/internal/provider/cricket"
	localprovider "github.com/qcrs/accelerator-control-plane/internal/provider/local"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

func main() {
	var metricsAddress string
	var probeAddress string
	var leaderElection bool
	flag.StringVar(&metricsAddress, "metrics-bind-address", ":8080", "Metrics endpoint address")
	flag.StringVar(&probeAddress, "health-probe-bind-address", ":8081", "Health probe endpoint address")
	flag.BoolVar(&leaderElection, "leader-elect", true, "Enable leader election")
	zapOptions := zap.Options{Development: true}
	zapOptions.BindFlags(flag.CommandLine)
	flag.Parse()
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&zapOptions)))

	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(corev1.AddToScheme(scheme))
	utilruntime.Must(acceleratorv1alpha1.AddToScheme(scheme))

	manager, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: metricsAddress},
		HealthProbeBindAddress: probeAddress,
		LeaderElection:         leaderElection,
		LeaderElectionID:       "accelerator-control-plane.qcrs.io",
	})
	exitOnError(err, "create manager")

	registry, err := contracts.NewProviderRegistry(
		localprovider.New(),
		cricket.New(),
	)
	exitOnError(err, "create provider registry")
	allocatorRouter := &allocation.Router{
		Local: &allocation.LocalAllocator{},
		Remote: &allocation.RemoteAllocator{
			Resolver:  &allocation.KubernetesEndpointResolver{Client: manager.GetClient()},
			Validator: registry,
		},
	}
	reconciler := &workloadcontroller.WorkloadReconciler{
		Client:    manager.GetClient(),
		Scheme:    manager.GetScheme(),
		Inventory: inventory.NewKubernetesReader(manager.GetClient(), registry),
		Policy:    policy.NewLocalFirst(),
		Allocator: allocatorRouter,
		Providers: registry,
		Injector:  injector.NewPodMutator(),
	}
	exitOnError(reconciler.SetupWithManager(manager), "register workload controller")
	exitOnError(manager.AddHealthzCheck("healthz", healthz.Ping), "register health check")
	exitOnError(manager.AddReadyzCheck("readyz", healthz.Ping), "register readiness check")
	exitOnError(manager.Start(ctrl.SetupSignalHandler()), "run manager")
}

func exitOnError(err error, operation string) {
	if err == nil {
		return
	}
	ctrl.Log.Error(err, operation)
	os.Exit(1)
}
