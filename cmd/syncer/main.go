/*
Copyright 2022 The MultiCluster Traffic Controller Authors.
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
	"strings"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/log"

	corev1 "k8s.io/api/core/v1"
	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	//+kubebuilder:scaffold:imports

	"github.com/Kuadrant/multi-cluster-traffic-controller/pkg/_internal/clusterSecret"
	"github.com/Kuadrant/multi-cluster-traffic-controller/pkg/syncer"
	"github.com/Kuadrant/multi-cluster-traffic-controller/pkg/syncer/mutator"
	"github.com/Kuadrant/multi-cluster-traffic-controller/pkg/syncer/spec"
	"github.com/Kuadrant/multi-cluster-traffic-controller/pkg/syncer/status"
)

var (
	scheme            = runtime.NewScheme()
	setupLog          = ctrl.Log.WithName("setup")
	NEVER_SYNCED_GVRs = []string{"pods"}
)

const (
	DEFAULT_RESYNC     = 0
	dataPlaneNamespace = "mctc-downstream"
)

type arrayFlags []string

func (i *arrayFlags) String() string {
	return strings.Join(*i, ",")
}

func (i *arrayFlags) Set(value string) error {
	*i = append(*i, value)
	return nil
}

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	//+kubebuilder:scaffold:scheme
}

func main() {
	var metricsAddr string
	var enableLeaderElection bool
	var probeAddr string
	var controlPlaneConfigSecretName string
	var controlPlaneConfigSecretNamespace string
	var syncedResources arrayFlags
	var controlPlaneNS string

	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "The address the metric endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.StringVar(&controlPlaneConfigSecretName, "control-plane-config-name", "control-plane-cluster", "The name of the secret with the control plane client configuration")
	flag.StringVar(&controlPlaneConfigSecretNamespace, "control-plane-config-namespace", "mctc-system", "The namespace containing the secret with the control plane client configuration")
	flag.Var(&syncedResources, "synced-resources", "A list of GVRs to sync (e.g. ingresses.v1.networking.k8s.io)")
	flag.StringVar(&controlPlaneNS, "control-plane-namespace", "mctc-tenant", "The name of the upstream namespace to sync resources from")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	opts := zap.Options{
		Development: true,
	}

	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		MetricsBindAddress:     metricsAddr,
		Port:                   9443,
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "fb80029c-sync.kuadrant.io",
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	syncerContext, cancel := context.WithCancel(context.Background())
	defer cancel()
	controlPlaneDynamicClient, controlPlaneConfig, err := getControlPlaneObjects(syncerContext, mgr, client.ObjectKey{Namespace: controlPlaneConfigSecretNamespace, Name: controlPlaneConfigSecretName})
	if err != nil {
		setupLog.Error(err, "error getting control plane secret")
		os.Exit(1)
	}
	dataPlaneDynamicClient, err := dynamic.NewForConfig(mgr.GetConfig())
	if err != nil {
		setupLog.Error(err, "unable to create client from mgr rest config")
		os.Exit(1)
	}
	syncRunnable, err := startSpecSyncers(syncerContext, syncedResources, controlPlaneDynamicClient, dataPlaneDynamicClient, controlPlaneConfig.Name, controlPlaneNS)
	if err != nil {
		setupLog.Error(err, "unable to start spec syncers")
		os.Exit(1)
	}

	if err := mgr.Add(syncRunnable); err != nil {
		setupLog.Error(err, "error starting spec syncers")
		os.Exit(1)
	}

	statusRunnable, err := startStatusSyncers(syncerContext, syncedResources, controlPlaneDynamicClient, dataPlaneDynamicClient, controlPlaneConfig.Name, controlPlaneNS)
	if err != nil {
		setupLog.Error(err, "unable to start status syncers")
		os.Exit(1)
	}

	if err := mgr.Add(statusRunnable); err != nil {
		setupLog.Error(err, "error starting status syncers")
		os.Exit(1)
	}

	setupLog.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}
func getControlPlaneObjects(ctx context.Context, mgr manager.Manager, secretRef client.ObjectKey) (dynamic.Interface, *clusterSecret.ClusterConfig, error) {
	controlPlaneSecret := &corev1.Secret{}
	secretClient, err := client.New(mgr.GetConfig(), client.Options{})
	if err != nil {
		setupLog.Error(err, "unable to create sync secret client")
		os.Exit(1)
	}
	err = secretClient.Get(ctx, secretRef, controlPlaneSecret, &client.GetOptions{})
	if err != nil {
		return nil, nil, fmt.Errorf("unable to retrieve control plane config secret '%v', error: %v", secretRef.String(), err)

	}
	controlPlaneClusterConfig, err := clusterSecret.ClusterConfigFromSecret(controlPlaneSecret)
	if err != nil {
		return nil, nil, fmt.Errorf("unable to create cluster config from control plane secret: %v", err)
	}
	controlPlaneClient, err := clusterSecret.DynamicClientsetFromSecret(controlPlaneSecret)
	if err != nil {
		return nil, nil, fmt.Errorf("unable to create client from control plane rest config: %v", err)
	}
	return controlPlaneClient, controlPlaneClusterConfig, nil
}
func startSpecSyncers(ctx context.Context, GVRs []string, controlPlaneClient dynamic.Interface, dataPlaneClient dynamic.Interface, clusterID, controlPlaneNS string) (*syncer.SyncRunnable, error) {
	logger := log.FromContext(ctx)
	informerFactory := dynamicinformer.NewDynamicSharedInformerFactory(controlPlaneClient, DEFAULT_RESYNC)
	informerFactory.Start(ctx.Done())
	informerFactory.WaitForCacheSync(ctx.Done())

	//for spec: upstream is control-plane, downstream is data plane
	specSyncConfig := syncer.Config{
		GVRs:               GVRs,
		InformerFactory:    informerFactory,
		ClusterID:          clusterID,
		NeverSyncedGVRs:    NEVER_SYNCED_GVRs,
		UpstreamNamespaces: []string{controlPlaneNS},
		DownstreamNS:       dataPlaneNamespace,
		Mutators: []syncer.Mutator{
			&mutator.JSONPatch{},
		},
	}

	SpecSyncer, err := spec.NewSpecSyncer(clusterID, controlPlaneClient, dataPlaneClient, specSyncConfig)
	if err != nil {
		return nil, fmt.Errorf("could not create new spec syncer: %v", err.Error())
	}
	logger.Info("starting spec syncer", "name", clusterID)

	go SpecSyncer.Start(ctx)

	syncRunnable := syncer.GetSyncerRunnable(specSyncConfig, syncer.InformerForGVR, SpecSyncer)
	return syncRunnable, nil

}

func startStatusSyncers(ctx context.Context, GVRs []string, controlPlaneClient dynamic.Interface, dataPlaneClient dynamic.Interface, clusterID, controlPlaneNS string) (*syncer.SyncRunnable, error) {
	logger := log.FromContext(ctx)

	informerFactory := dynamicinformer.NewDynamicSharedInformerFactory(dataPlaneClient, DEFAULT_RESYNC)
	informerFactory.Start(ctx.Done())
	informerFactory.WaitForCacheSync(ctx.Done())

	//for status: downstream is control-plane, upstream is data plane
	statusSyncConfig := syncer.Config{
		GVRs:               GVRs,
		InformerFactory:    informerFactory,
		ClusterID:          clusterID,
		NeverSyncedGVRs:    NEVER_SYNCED_GVRs,
		UpstreamNamespaces: []string{dataPlaneNamespace},
		DownstreamNS:       controlPlaneNS,
	}

	statusSyncer, err := status.NewStatusSyncer(clusterID, dataPlaneClient, controlPlaneClient, statusSyncConfig)
	if err != nil {
		return nil, fmt.Errorf("could not create new spec syncer: %v", err.Error())
	}
	logger.Info("starting status syncer", "name", clusterID)

	go statusSyncer.Start(ctx)

	syncRunnable := syncer.GetSyncerRunnable(statusSyncConfig, syncer.InformerForGVR, statusSyncer)
	return syncRunnable, nil
}
