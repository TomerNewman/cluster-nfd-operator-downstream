/*

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
	"github.com/go-logr/logr"
	_ "k8s.io/client-go/plugin/pkg/client/auth"
	"k8s.io/klog/v2/textlogger"
	"os"
	"sigs.k8s.io/controller-runtime/pkg/cache"

	securityscheme "github.com/openshift/client-go/security/clientset/versioned/scheme"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	nfdopenshiftv1 "github.com/openshift/cluster-nfd-operator/api/v1"
	nfdopenshiftiov1alpha1 "github.com/openshift/cluster-nfd-operator/api/v1alpha1"
	nfdk8ssigsioalpha1 "github.com/openshift/cluster-nfd-operator/api/v1temp1"
	"github.com/openshift/cluster-nfd-operator/internal/configmap"
	new_controllers "github.com/openshift/cluster-nfd-operator/internal/controllers"
	"github.com/openshift/cluster-nfd-operator/internal/daemonset"
	"github.com/openshift/cluster-nfd-operator/internal/deployment"
	"github.com/openshift/cluster-nfd-operator/internal/job"
	"github.com/openshift/cluster-nfd-operator/internal/scc"
	"github.com/openshift/cluster-nfd-operator/internal/status"
	// +kubebuilder:scaffold:imports
)

var (
	// scheme holds a new scheme for the operator
	scheme  = runtime.NewScheme()
	version = "undefined"
)

const (
	// ProgramName is the canonical name of this program
	ProgramName          = "nfd-operator"
	watchNamespaceEnvVar = "POD_NAMESPACE"
)

// operatorArgs holds command line arguments
type operatorArgs struct {
	metricsAddr                string
	enableLeaderElection       bool
	conversionManagerProbeAddr string
	probeAddr                  string
}

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(securityscheme.AddToScheme(scheme))

	utilruntime.Must(nfdopenshiftv1.AddToScheme(scheme))
	utilruntime.Must(nfdopenshiftiov1alpha1.AddToScheme(scheme))
	utilruntime.Must(nfdk8ssigsioalpha1.AddToScheme(scheme))
	// +kubebuilder:scaffold:scheme
}

func main() {
	// probeAddr is responsible for the health probe bind address, where the health
	// probe is responsible for determining liveness, readiness, and configuration
	// of the operator pods. Note that the port which is being binded must match
	// the bind port under './config' and './manifests'
	probeAddr := ":8081"

	flags := flag.NewFlagSet(ProgramName, flag.ExitOnError)

	printVersion := flags.Bool("version", false, "Print version and exit.")

	args := initFlags(flags)

	logConfig := textlogger.NewConfig()
	logConfig.AddFlags(flag.CommandLine)
	logger := textlogger.NewLogger(logConfig).WithName("nfd")

	ctrl.SetLogger(logger)
	setupLogger := logger.WithName("setup")

	_ = flags.Parse(os.Args[1:])
	if len(flags.Args()) > 0 {
		setupLogger.Info("unknown command line argument", flags.Args()[0])
		flags.Usage()
		os.Exit(2)
	}

	if *printVersion {
		fmt.Println(ProgramName, version)
		os.Exit(0)
	}
	watchNamespace, err := getWatchNamespace()
	if err != nil {
		setupLogger.Error(err, "WatchNamespaceEnvVar is not set")
		os.Exit(1)
	}

	metricsOptions := metricsserver.Options{
		BindAddress:    args.metricsAddr,
		SecureServing:  true,
		FilterProvider: filters.WithAuthenticationAndAuthorization,
	}

	// Create a new manager to manage the operator
	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:  scheme,
		Metrics: metricsOptions,
		WebhookServer: webhook.NewServer(webhook.Options{
			Port: 9443,
		}),
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         args.enableLeaderElection,
		LeaderElectionID:       "39f5e5c3.nodefeaturediscoveries.nfd.openshift.io",
		Cache: cache.Options{
			DefaultNamespaces: map[string]cache.Config{
				watchNamespace: cache.Config{},
			},
		},
	})

	if err != nil {
		setupLogger.Error(err, "unable to start manager")
		os.Exit(1)
	}

	client := mgr.GetClient()
	scheme := mgr.GetScheme()

	deploymentAPI := deployment.NewDeploymentAPI(client, scheme)
	daemonsetAPI := daemonset.NewDaemonsetAPI(client, scheme)
	configmapAPI := configmap.NewConfigMapAPI(client, scheme)
	jobAPI := job.NewJobAPI(client, scheme)
	sccAPI := scc.NewSccAPI(client, scheme)
	statusAPI := status.NewStatusAPI(deploymentAPI, daemonsetAPI)

	if err = new_controllers.NewNodeFeatureDiscoveryReconciler(client,
		deploymentAPI,
		daemonsetAPI,
		configmapAPI,
		jobAPI,
		sccAPI,
		statusAPI,
		scheme).SetupWithManager(mgr); err != nil {
		setupLogger.Error(err, "unable to create controller", "controller", "NodeFeatureDiscovery")
		os.Exit(1)
	}
	stopCh := ctrl.SetupSignalHandler()
	conversionInitialization(args.conversionManagerProbeAddr, setupLogger, stopCh)
	// +kubebuilder:scaffold:builder

	// Next, add a Healthz checker to the manager. Healthz is a health and liveness package
	// that the operator will use to periodically check the health of its pods, etc.
	if err := mgr.AddHealthzCheck("health", healthz.Ping); err != nil {
		setupLogger.Error(err, "unable to set up health check")
		os.Exit(1)
	}

	// Now add a ReadyZ checker to the manager as well. It is important to ensure that the
	// API server's readiness is checked when the operator is installed and running.
	if err := mgr.AddReadyzCheck("check", healthz.Ping); err != nil {
		setupLogger.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	// Register signal handler for SIGINT and SIGTERM to terminate the manager
	setupLogger.Info("starting manager")
	if err := mgr.Start(stopCh); err != nil {
		setupLogger.Error(err, "problem running manager")
		os.Exit(1)
	}
}

func initFlags(flagset *flag.FlagSet) *operatorArgs {
	args := operatorArgs{}

	// Setup CLI arguments
	flagset.StringVar(&args.metricsAddr, "metrics-bind-address", ":8080", "The address the Prometheus "+
		"metric endpoint binds to for scraping NFD resource usage data.")
	flagset.StringVar(&args.probeAddr, "health-probe-bind-address", ":8081", "The address the probe "+
		"endpoint binds to for determining liveness, readiness, and configuration of"+
		"operator pods.")
	flagset.StringVar(&args.conversionManagerProbeAddr, "conversion-manager-health-probe-bind-address", ":8082",
		"The address the probe endpoint binds to for determining liveness/readiness of the global controller.")
	flagset.BoolVar(&args.enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")

	return &args
}

// getWatchNamespace returns the Namespace the operator should be watching for changes
func getWatchNamespace() (string, error) {
	value, present := os.LookupEnv(watchNamespaceEnvVar)
	if !present {
		return "", fmt.Errorf("environment variable %s is not defined", watchNamespaceEnvVar)
	}
	return value, nil
}

func conversionInitialization(conversionManagerProbeAddr string, setupLogger logr.Logger, stopCh context.Context) {
	conversionMgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		HealthProbeBindAddress: conversionManagerProbeAddr,
		Metrics:                metricsserver.Options{BindAddress: "0"},
		LeaderElection:         false,
	})
	if err != nil {
		setupLogger.Error(err, "unable to start manager for NodeFeatureRule controller")
		os.Exit(1)
	}

	if err = new_controllers.NewNodeFeatureRuleReconciler(conversionMgr.GetClient(), conversionMgr.GetScheme()).
		SetupWithManager(conversionMgr); err != nil {
		setupLogger.Error(err, "unable to create NodeFeatureRule controller")
		os.Exit(1)
	}
	go func() {
		if err := conversionMgr.Start(stopCh); err != nil {
			setupLogger.Error(err, "problem running manager for NodeFeatureRule controller")
			os.Exit(1)
		}
	}()
}
