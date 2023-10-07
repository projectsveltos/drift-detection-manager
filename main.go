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
	"k8s.io/klog/v2/klogr"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"

	"github.com/projectsveltos/drift-detection-manager/controllers"
	driftdetection "github.com/projectsveltos/drift-detection-manager/pkg/drift-detection"
	libsveltosv1alpha1 "github.com/projectsveltos/libsveltos/api/v1alpha1"
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
	setupLog         = ctrl.Log.WithName("setup")
	metricsAddr      string
	probeAddr        string
	runMode          string
	deployedCluster  string
	clusterNamespace string
	clusterName      string
	clusterType      string
)

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

	cfg := ctrl.GetConfigOrDie()
	if deployedCluster != managedCluster {
		// if drift-detection-manager is running in the management cluster, get the kubeconfig
		// of the managed cluster
		cfg = getManagedClusterRestConfig(ctx, cfg, ctrl.Log.WithName("get-kubeconfig"))
	}
	cfg.Burst = 60
	cfg.QPS = 40

	logsettings.RegisterForLogSettings(ctx,
		libsveltosv1alpha1.ComponentDriftDetectionManager, ctrl.Log.WithName("log-setter"),
		cfg)

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:                 scheme,
		MetricsBindAddress:     metricsAddr,
		Port:                   9443,
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         false,
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

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
		ClusterType:            libsveltosv1alpha1.ClusterType(clusterType),
	}).SetupWithManager(ctx, mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "ResourceSummary")
		os.Exit(1)
	}
	//+kubebuilder:scaffold:builder

	setupChecks(mgr)

	go initializeManager(ctx, mgr, sendUpdates, clusterNamespace, clusterName,
		libsveltosv1alpha1.ClusterType(clusterType), setupLog)

	setupLog.Info("starting manager")
	if err := mgr.Start(ctx); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}

func initFlags(fs *pflag.FlagSet) {
	fs.StringVar(&metricsAddr,
		"metrics-bind-address",
		":8080",
		"The address the metric endpoint binds to.")

	fs.StringVar(&probeAddr,
		"health-probe-bind-address",
		":8081",
		"The address the probe endpoint binds to.")

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
	clusterNamespace, clusterName string, clusterType libsveltosv1alpha1.ClusterType,
	logger logr.Logger) {

	const intervalInSecond = 10

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
	if err := libsveltosv1alpha1.AddToScheme(s); err != nil {
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
		libsveltosv1alpha1.ClusterType(clusterType), klogr.New())
	if err != nil {
		logger.V(logsettings.LogInfo).Info(fmt.Sprintf("failed to get secret: %v", err))
		panic(1)
	}

	return currentCfg
}
