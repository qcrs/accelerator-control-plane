package main

import (
	"flag"
	"os"

	acceleratorv1alpha1 "github.com/qcrs/accelerator-control-plane/api/v1alpha1"
	executionadapter "github.com/qcrs/accelerator-control-plane/internal/adapter"
	"github.com/qcrs/accelerator-control-plane/internal/allocation"
	workloadcontroller "github.com/qcrs/accelerator-control-plane/internal/controller"
	"github.com/qcrs/accelerator-control-plane/internal/injector"
	"github.com/qcrs/accelerator-control-plane/internal/inventory"
	"github.com/qcrs/accelerator-control-plane/internal/policy"
	corexprovider "github.com/qcrs/accelerator-control-plane/internal/provider/corex"
	"github.com/qcrs/accelerator-control-plane/internal/provider/cricket"
	localprovider "github.com/qcrs/accelerator-control-plane/internal/provider/local"
	mluprovider "github.com/qcrs/accelerator-control-plane/internal/provider/mlu"
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
	var coordinationNamespace string
	var leaderElection bool
	flag.StringVar(&metricsAddress, "metrics-bind-address", ":8080", "Metrics endpoint address")
	flag.StringVar(&probeAddress, "health-probe-bind-address", ":8081", "Health probe endpoint address")
	flag.StringVar(&coordinationNamespace, "coordination-namespace", "accelerator-system", "Namespace used for leader election and atomic remote endpoint claims")
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
		Scheme:                  scheme,
		Metrics:                 metricsserver.Options{BindAddress: metricsAddress},
		HealthProbeBindAddress:  probeAddress,
		LeaderElection:          leaderElection,
		LeaderElectionID:        "accelerator-control-plane.qcrs.io",
		LeaderElectionNamespace: coordinationNamespace,
	})
	exitOnError(err, "create manager")

	remoteAllocator := &allocation.RemoteAllocator{
		// Inventory is intentionally cache-backed, but the post-claim Endpoint
		// snapshot must be read directly from the API server so a stale cache
		// cannot materialize a recently changed address or runtime.
		Resolver:  &allocation.KubernetesEndpointResolver{Client: manager.GetAPIReader()},
		Validator: nil,
		Claimer: &allocation.KubernetesEndpointClaimer{
			Client:    manager.GetClient(),
			APIReader: manager.GetAPIReader(),
			Namespace: coordinationNamespace,
		},
	}
	adapters, err := executionadapter.NewRegistry(
		executionadapter.NewBinding(localprovider.New(), &allocation.LocalAllocator{}),
		executionadapter.NewBinding(cricket.New(), remoteAllocator),
		executionadapter.NewBinding(mluprovider.New(), remoteAllocator),
		executionadapter.NewBinding(corexprovider.New(), remoteAllocator),
	)
	exitOnError(err, "create provider registry")
	registry := adapters.Providers()
	remoteAllocator.Validator = registry
	recorder := manager.GetEventRecorderFor("accelerator-control-plane")
	reconciler := &workloadcontroller.WorkloadReconciler{
		Client:    manager.GetClient(),
		APIReader: manager.GetAPIReader(),
		Scheme:    manager.GetScheme(),
		Inventory: inventory.NewKubernetesReader(manager.GetClient(), registry),
		Policy:    policy.NewLocalFirst(),
		Adapters:  adapters,
		Injector:  injector.NewPodMutator(),
		Recorder:  recorder,
	}
	exitOnError(reconciler.SetupWithManager(manager), "register workload controller")
	exitOnError((&workloadcontroller.ProviderReconciler{
		Client:    manager.GetClient(),
		Providers: registry,
		Recorder:  recorder,
	}).SetupWithManager(manager), "register provider configuration controller")
	exitOnError((&workloadcontroller.PoolReconciler{
		Client:    manager.GetClient(),
		Providers: registry,
		Recorder:  recorder,
	}).SetupWithManager(manager), "register pool configuration controller")
	exitOnError((&workloadcontroller.EndpointReconciler{
		Client:    manager.GetClient(),
		Providers: registry,
		Recorder:  recorder,
	}).SetupWithManager(manager), "register endpoint configuration controller")
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
