/*
Copyright 2022.

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

package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"sync"
	"time"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	"github.com/go-logr/logr"
	"github.com/spf13/pflag"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	_ "k8s.io/client-go/plugin/pkg/client/auth"
	"k8s.io/client-go/rest"
	cliflag "k8s.io/component-base/cli/flag"
	"k8s.io/klog/v2"
	"k8s.io/klog/v2/textlogger"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	"github.com/projectsveltos/drift-detection-manager/controllers"
	driftdetection "github.com/projectsveltos/drift-detection-manager/pkg/drift-detection"
	libsveltosv1beta1 "github.com/projectsveltos/libsveltos/api/v1beta1"
	"github.com/projectsveltos/libsveltos/lib/clusterproxy"
	"github.com/projectsveltos/libsveltos/lib/logsettings"
	libsveltosset "github.com/projectsveltos/libsveltos/lib/set"
	//+kubebuilder:scaffold:imports
)

const (
	noUpdates = "do-not-send-updates"

	managedCluster = "managed-cluster"
)

var (
	setupLog            = ctrl.Log.WithName("setup")
	diagnosticsAddress  string
	insecureDiagnostics bool
	runMode             string
	deployedCluster     string
	clusterNamespace    string
	clusterName         string
	clusterType         string
	restConfigQPS       float32
	restConfigBurst     int
	webhookPort         int
	syncPeriod          time.Duration
	healthAddr          string
)

// Add RBAC for the authorized diagnostics endpoint.
// +kubebuilder:rbac:groups=authentication.k8s.io,resources=tokenreviews,verbs=create
// +kubebuilder:rbac:groups=authorization.k8s.io,resources=subjectaccessreviews,verbs=create

func main() {
	scheme, err := controllers.InitScheme()
	if err != nil {
		os.Exit(1)
	}

	klog.InitFlags(nil)

	initFlags(pflag.CommandLine)
	pflag.CommandLine.SetNormalizeFunc(cliflag.WordSepNormalizeFunc)
	pflag.CommandLine.AddGoFlagSet(flag.CommandLine)
	pflag.Parse()

	ctrl.SetLogger(klog.Background())

	ctx := ctrl.SetupSignalHandler()

	ctrlOptions := ctrl.Options{
		Scheme:                 scheme,
		Metrics:                getDiagnosticsOptions(),
		HealthProbeBindAddress: healthAddr,
		WebhookServer: webhook.NewServer(
			webhook.Options{
				Port: webhookPort,
			}),
		Cache: cache.Options{
			SyncPeriod: &syncPeriod,
		},
	}

	restConfig := ctrl.GetConfigOrDie()
	if deployedCluster != managedCluster {
		// if drift-detection-manager is running in the management cluster, get the kubeconfig
		// of the managed cluster
		restConfig = getManagedClusterRestConfig(ctx, restConfig, ctrl.Log.WithName("get-kubeconfig"))
	}
	restConfig.QPS = restConfigQPS
	restConfig.Burst = restConfigBurst

	mgr, err := ctrl.NewManager(restConfig, ctrlOptions)
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	logsettings.RegisterForLogSettings(ctx,
		libsveltosv1beta1.ComponentDriftDetectionManager, ctrl.Log.WithName("log-setter"),
		restConfig)

	sendUpdates := controllers.SendUpdates // do not send reports
	if runMode == noUpdates {
		sendUpdates = controllers.DoNotSendUpdates
	}

	if err = (&controllers.ResourceSummaryReconciler{
		Client:                 mgr.GetClient(),
		Config:                 mgr.GetConfig(),
		Scheme:                 mgr.GetScheme(),
		RunMode:                sendUpdates,
		Mux:                    sync.RWMutex{},
		ResourceSummaryMap:     make(map[corev1.ObjectReference]*libsveltosset.Set),
		HelmResourceSummaryMap: make(map[corev1.ObjectReference]*libsveltosset.Set),
		ClusterNamespace:       clusterNamespace,
		ClusterName:            clusterName,
		ClusterType:            libsveltosv1beta1.ClusterType(clusterType),
		MapperLock:             sync.Mutex{},
	}).SetupWithManager(ctx, mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "ResourceSummary")
		os.Exit(1)
	}
	//+kubebuilder:scaffold:builder

	setupChecks(mgr)

	go initializeManager(ctx, mgr, sendUpdates, clusterNamespace, clusterName,
		libsveltosv1beta1.ClusterType(clusterType), setupLog)

	setupLog.Info("starting manager")
	if err := mgr.Start(ctx); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}

func initFlags(fs *pflag.FlagSet) {
	fs.StringVar(&diagnosticsAddress, "diagnostics-address", ":8443",
		"The address the diagnostics endpoint binds to. Per default metrics are served via https and with"+
			"authentication/authorization. To serve via http and without authentication/authorization set --insecure-diagnostics."+
			"If --insecure-diagnostics is not set the diagnostics endpoint also serves pprof endpoints and an endpoint to change the log level.")

	fs.BoolVar(&insecureDiagnostics, "insecure-diagnostics", false,
		"Enable insecure diagnostics serving. For more details see the description of --diagnostics-address.")

	flag.StringVar(
		&runMode,
		"run-mode",
		noUpdates,
		"indicates whether updates will be sent to management cluster or just created locally",
	)

	flag.StringVar(
		&deployedCluster,
		"current-cluster",
		managedCluster,
		"Indicate whether drift-detection-manager was deployed in the managed or the management cluster. "+
			"Possible options are managed-cluster or management-cluster.",
	)

	flag.StringVar(
		&clusterNamespace,
		"cluster-namespace",
		"",
		"cluster namespace",
	)

	flag.StringVar(
		&clusterName,
		"cluster-name",
		"",
		"cluster name",
	)

	flag.StringVar(
		&clusterType,
		"cluster-type",
		"",
		"cluster type",
	)

	fs.StringVar(&healthAddr, "health-addr", ":9440",
		"The address the health endpoint binds to.")

	const defautlRestConfigQPS = 40
	fs.Float32Var(&restConfigQPS, "kube-api-qps", defautlRestConfigQPS,
		fmt.Sprintf("Maximum queries per second from the controller client to the Kubernetes API server. Defaults to %d",
			defautlRestConfigQPS))

	const defaultRestConfigBurst = 60
	fs.IntVar(&restConfigBurst, "kube-api-burst", defaultRestConfigBurst,
		fmt.Sprintf("Maximum number of queries that should be allowed in one burst from the controller client to the Kubernetes API server. Default %d",
			defaultRestConfigBurst))

	const defaultWebhookPort = 9443
	fs.IntVar(&webhookPort, "webhook-port", defaultWebhookPort,
		"Webhook Server port")

	const defaultSyncPeriod = 10
	fs.DurationVar(&syncPeriod, "sync-period", defaultSyncPeriod*time.Minute,
		fmt.Sprintf("The minimum interval at which watched resources are reconciled (e.g. 15m). Default: %d minutes",
			defaultSyncPeriod))
}

func setupChecks(mgr ctrl.Manager) {
	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}
}

func initializeManager(ctx context.Context, mgr ctrl.Manager, sendUpdates controllers.Mode,
	clusterNamespace, clusterName string, clusterType libsveltosv1beta1.ClusterType,
	logger logr.Logger) {

	const intervalInSecond = 5

	for {
		var err error
		if sendUpdates == controllers.SendUpdates {
			err = driftdetection.InitializeManager(ctx, mgr.GetLogger(), mgr.GetConfig(), mgr.GetClient(), mgr.GetScheme(),
				clusterNamespace, clusterName, clusterType, intervalInSecond, true)
		} else {
			err = driftdetection.InitializeManager(ctx, mgr.GetLogger(), mgr.GetConfig(), mgr.GetClient(), mgr.GetScheme(),
				clusterNamespace, clusterName, clusterType, intervalInSecond, false)
		}

		if err != nil {
			logger.V(logsettings.LogInfo).Info(fmt.Sprintf("failed to initialize manager %v", err))
			time.Sleep(time.Second)
			continue
		}

		logger.V(logsettings.LogInfo).Info("manager initialized")
		break
	}
}

func getManagedClusterRestConfig(ctx context.Context, cfg *rest.Config, logger logr.Logger) *rest.Config {
	logger = logger.WithValues("cluster", fmt.Sprintf("%s:%s/%s", clusterType, clusterNamespace, clusterName))
	logger.V(logsettings.LogInfo).Info("get secret with kubeconfig")

	// When running in the management cluster, drift-detection-manager will need
	// to access Secret and Cluster/SveltosCluster (to verify existence)
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		panic(1)
	}
	if err := clusterv1.AddToScheme(s); err != nil {
		panic(1)
	}
	if err := libsveltosv1beta1.AddToScheme(s); err != nil {
		panic(1)
	}

	c, err := client.New(cfg, client.Options{Scheme: s})
	if err != nil {
		logger.V(logsettings.LogInfo).Info(fmt.Sprintf("failed to get management cluster client: %v", err))
		panic(1)
	}

	// In this mode, drift-detection-manager is running in the management cluster.
	// It access the managed cluster from here.
	var currentCfg *rest.Config
	currentCfg, err = clusterproxy.GetKubernetesRestConfig(ctx, c, clusterNamespace, clusterName, "", "",
		libsveltosv1beta1.ClusterType(clusterType), textlogger.NewLogger(textlogger.NewConfig(textlogger.Verbosity(1))))
	if err != nil {
		logger.V(logsettings.LogInfo).Info(fmt.Sprintf("failed to get secret: %v", err))
		panic(1)
	}

	return currentCfg
}

// getDiagnosticsOptions returns metrics options which can be used to configure a Manager.
func getDiagnosticsOptions() metricsserver.Options {
	// If "--insecure-diagnostics" is set, serve metrics via http
	// and without authentication/authorization.
	if insecureDiagnostics {
		return metricsserver.Options{
			BindAddress:   diagnosticsAddress,
			SecureServing: false,
		}
	}

	// If "--insecure-diagnostics" is not set, serve metrics via https
	// and with authentication/authorization. As the endpoint is protected,
	// we also serve pprof endpoints and an endpoint to change the log level.
	return metricsserver.Options{
		BindAddress:    diagnosticsAddress,
		SecureServing:  true,
		FilterProvider: filters.WithAuthenticationAndAuthorization,
	}
}
