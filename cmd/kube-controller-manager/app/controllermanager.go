/*
Copyright 2014 The Kubernetes Authors All rights reserved.

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

// Package app implements a server that runs a set of active
// components.  This includes replication controllers, service endpoints and
// nodes.
package app

import (
	"net"
	"net/http"
	"net/http/pprof"
	"strconv"
	"time"

	"github.com/GoogleCloudPlatform/kubernetes/pkg/api"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/api/resource"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/client"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/client/clientcmd"
	clientcmdapi "github.com/GoogleCloudPlatform/kubernetes/pkg/client/clientcmd/api"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/cloudprovider"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/cloudprovider/nodecontroller"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/cloudprovider/servicecontroller"
	replicationControllerPkg "github.com/GoogleCloudPlatform/kubernetes/pkg/controller"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/healthz"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/master/ports"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/namespace"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/resourcequota"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/service"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/serviceaccount"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/util"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/volumeclaimbinder"

	"github.com/golang/glog"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/spf13/pflag"
)

// CMServer is the main context object for the controller manager.
type CMServer struct {
	Port                    int
	Address                 util.IP
	CloudProvider           string
	CloudConfigFile         string
	ConcurrentEndpointSyncs int
	ConcurrentRCSyncs       int
	MinionRegexp            string
	NodeSyncPeriod          time.Duration
	ResourceQuotaSyncPeriod time.Duration
	NamespaceSyncPeriod     time.Duration
	PVClaimBinderSyncPeriod time.Duration
	RegisterRetryCount      int
	MachineList             util.StringList
	SyncNodeList            bool
	SyncNodeStatus          bool
	NodeMonitorGracePeriod  time.Duration
	NodeStartupGracePeriod  time.Duration
	NodeMonitorPeriod       time.Duration
	NodeStatusUpdateRetry   int
	PodEvictionTimeout      time.Duration
	DeletingPodsQps         float32
	DeletingPodsBurst       int
	ServiceAccountKeyFile   string

	// TODO: Discover these by pinging the host machines, and rip out these params.
	NodeMilliCPU int64
	NodeMemory   resource.Quantity

	ClusterName       string
	ClusterCIDR       util.IPNet
	AllocateNodeCIDRs bool
	EnableProfiling   bool

	Master     string
	Kubeconfig string
}

// NewCMServer creates a new CMServer with a default config.
func NewCMServer() *CMServer {
	s := CMServer{
		Port:                    ports.ControllerManagerPort,
		Address:                 util.IP(net.ParseIP("127.0.0.1")),
		ConcurrentEndpointSyncs: 5,
		ConcurrentRCSyncs:       5,
		NodeSyncPeriod:          10 * time.Second,
		ResourceQuotaSyncPeriod: 10 * time.Second,
		NamespaceSyncPeriod:     5 * time.Minute,
		PVClaimBinderSyncPeriod: 10 * time.Second,
		RegisterRetryCount:      10,
		PodEvictionTimeout:      5 * time.Minute,
		NodeMilliCPU:            1000,
		NodeMemory:              resource.MustParse("3Gi"),
		SyncNodeList:            true,
		ClusterName:             "kubernetes",
	}
	return &s
}

// AddFlags adds flags for a specific CMServer to the specified FlagSet
func (s *CMServer) AddFlags(fs *pflag.FlagSet) {
	fs.IntVar(&s.Port, "port", s.Port, "The port that the controller-manager's http service runs on")
	fs.Var(&s.Address, "address", "The IP address to serve on (set to 0.0.0.0 for all interfaces)")
	fs.StringVar(&s.CloudProvider, "cloud-provider", s.CloudProvider, "The provider for cloud services.  Empty string for no provider.")
	fs.StringVar(&s.CloudConfigFile, "cloud-config", s.CloudConfigFile, "The path to the cloud provider configuration file.  Empty string for no configuration file.")
	fs.IntVar(&s.ConcurrentEndpointSyncs, "concurrent-endpoint-syncs", s.ConcurrentEndpointSyncs, "The number of endpoint syncing operations that will be done concurrently. Larger number = faster endpoint updating, but more CPU (and network) load")
	fs.IntVar(&s.ConcurrentRCSyncs, "concurrent_rc_syncs", s.ConcurrentRCSyncs, "The number of replication controllers that are allowed to sync concurrently. Larger number = more reponsive replica management, but more CPU (and network) load")
	fs.StringVar(&s.MinionRegexp, "minion-regexp", s.MinionRegexp, "If non empty, and --cloud-provider is specified, a regular expression for matching minion VMs.")
	fs.DurationVar(&s.NodeSyncPeriod, "node-sync-period", s.NodeSyncPeriod, ""+
		"The period for syncing nodes from cloudprovider. Longer periods will result in "+
		"fewer calls to cloud provider, but may delay addition of new nodes to cluster.")
	fs.DurationVar(&s.ResourceQuotaSyncPeriod, "resource-quota-sync-period", s.ResourceQuotaSyncPeriod, "The period for syncing quota usage status in the system")
	fs.DurationVar(&s.NamespaceSyncPeriod, "namespace-sync-period", s.NamespaceSyncPeriod, "The period for syncing namespace life-cycle updates")
	fs.DurationVar(&s.PVClaimBinderSyncPeriod, "pvclaimbinder-sync-period", s.PVClaimBinderSyncPeriod, "The period for syncing persistent volumes and persistent volume claims")
	fs.DurationVar(&s.PodEvictionTimeout, "pod-eviction-timeout", s.PodEvictionTimeout, "The grace peroid for deleting pods on failed nodes.")
	fs.Float32Var(&s.DeletingPodsQps, "deleting-pods-qps", 0.1, "Number of nodes per second on which pods are deleted in case of node failure.")
	fs.IntVar(&s.DeletingPodsBurst, "deleting-pods-burst", 10, "Number of nodes on which pods are bursty deleted in case of node failure. For more details look into RateLimiter.")
	fs.IntVar(&s.RegisterRetryCount, "register-retry-count", s.RegisterRetryCount, ""+
		"The number of retries for initial node registration.  Retry interval equals node-sync-period.")
	fs.Var(&s.MachineList, "machines", "List of machines to schedule onto, comma separated.")
	fs.BoolVar(&s.SyncNodeList, "sync-nodes", s.SyncNodeList, "If true, and --cloud-provider is specified, sync nodes from the cloud provider. Default true.")
	fs.BoolVar(&s.SyncNodeStatus, "sync-node-status", s.SyncNodeStatus,
		"DEPRECATED. Does not have any effect now and it will be removed in a later release.")
	fs.DurationVar(&s.NodeMonitorGracePeriod, "node-monitor-grace-period", 40*time.Second,
		"Amount of time which we allow running Node to be unresponsive before marking it unhealty. "+
			"Must be N times more than kubelet's nodeStatusUpdateFrequency, "+
			"where N means number of retries allowed for kubelet to post node status.")
	fs.DurationVar(&s.NodeStartupGracePeriod, "node-startup-grace-period", 60*time.Second,
		"Amount of time which we allow starting Node to be unresponsive before marking it unhealty.")
	fs.DurationVar(&s.NodeMonitorPeriod, "node-monitor-period", 5*time.Second,
		"The period for syncing NodeStatus in NodeController.")
	fs.StringVar(&s.ServiceAccountKeyFile, "service-account-private-key-file", s.ServiceAccountKeyFile, "Filename containing a PEM-encoded private RSA key used to sign service account tokens.")
	// TODO: Discover these by pinging the host machines, and rip out these flags.
	// TODO: in the meantime, use resource.QuantityFlag() instead of these
	fs.Int64Var(&s.NodeMilliCPU, "node-milli-cpu", s.NodeMilliCPU, "The amount of MilliCPU provisioned on each node")
	fs.Var(resource.NewQuantityFlagValue(&s.NodeMemory), "node-memory", "The amount of memory (in bytes) provisioned on each node")
	fs.BoolVar(&s.EnableProfiling, "profiling", true, "Enable profiling via web interface host:port/debug/pprof/")
	fs.Var(&s.ClusterCIDR, "cluster-cidr", "CIDR Range for Pods in cluster.")
	fs.BoolVar(&s.AllocateNodeCIDRs, "allocate-node-cidrs", false, "Should CIDRs for Pods be allocated and set on the cloud provider.")
	fs.StringVar(&s.Master, "master", s.Master, "The address of the Kubernetes API server (overrides any value in kubeconfig)")
	fs.StringVar(&s.Kubeconfig, "kubeconfig", s.Kubeconfig, "Path to kubeconfig file with authorization and master location information.")
}

func (s *CMServer) verifyMinionFlags() {
	if !s.SyncNodeList && s.MinionRegexp != "" {
		glog.Info("--minion-regexp is ignored by --sync-nodes=false")
	}
	if s.CloudProvider == "" || s.MinionRegexp == "" {
		if len(s.MachineList) == 0 {
			glog.Info("No machines specified!")
		}
		return
	}
	if len(s.MachineList) != 0 {
		glog.Info("--machines is overwritten by --minion-regexp")
	}
}

// Run runs the CMServer.  This should never exit.
func (s *CMServer) Run(_ []string) error {
	s.verifyMinionFlags()

	if s.Kubeconfig == "" && s.Master == "" {
		glog.Warningf("Neither --kubeconfig nor --master was specified.  Using default API client.  This might not work.")
	}

	// This creates a client, first loading any specified kubeconfig
	// file, and then overriding the Master flag, if non-empty.
	kubeconfig, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		&clientcmd.ClientConfigLoadingRules{ExplicitPath: s.Kubeconfig},
		&clientcmd.ConfigOverrides{ClusterInfo: clientcmdapi.Cluster{Server: s.Master}}).ClientConfig()
	if err != nil {
		return err
	}

	kubeconfig.QPS = 20.0
	kubeconfig.Burst = 30

	kubeClient, err := client.New(kubeconfig)
	if err != nil {
		glog.Fatalf("Invalid API configuration: %v", err)
	}

	go func() {
		mux := http.NewServeMux()
		healthz.InstallHandler(mux)
		if s.EnableProfiling {
			mux.HandleFunc("/debug/pprof/", pprof.Index)
			mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
			mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
		}
		mux.Handle("/metrics", prometheus.Handler())

		server := &http.Server{
			Addr:    net.JoinHostPort(s.Address.String(), strconv.Itoa(s.Port)),
			Handler: mux,
		}
		glog.Fatal(server.ListenAndServe())
	}()

	endpoints := service.NewEndpointController(kubeClient)
	go endpoints.Run(s.ConcurrentEndpointSyncs, util.NeverStop)

	controllerManager := replicationControllerPkg.NewReplicationManager(kubeClient, replicationControllerPkg.BurstReplicas)
	go controllerManager.Run(s.ConcurrentRCSyncs, util.NeverStop)

	cloud := cloudprovider.InitCloudProvider(s.CloudProvider, s.CloudConfigFile)
	nodeResources := &api.NodeResources{
		Capacity: api.ResourceList{
			api.ResourceCPU:    *resource.NewMilliQuantity(s.NodeMilliCPU, resource.DecimalSI),
			api.ResourceMemory: s.NodeMemory,
		},
	}

	if s.SyncNodeStatus {
		glog.Warning("DEPRECATION NOTICE: sync-node-status flag is being deprecated. It has no effect now and it will be removed in a future version.")
	}

	nodeController := nodecontroller.NewNodeController(cloud, s.MinionRegexp, s.MachineList, nodeResources,
		kubeClient, s.RegisterRetryCount, s.PodEvictionTimeout, util.NewTokenBucketRateLimiter(s.DeletingPodsQps, s.DeletingPodsBurst),
		s.NodeMonitorGracePeriod, s.NodeStartupGracePeriod, s.NodeMonitorPeriod, (*net.IPNet)(&s.ClusterCIDR), s.AllocateNodeCIDRs)
	nodeController.Run(s.NodeSyncPeriod, s.SyncNodeList)

	serviceController := servicecontroller.New(cloud, kubeClient, s.ClusterName)
	if err := serviceController.Run(s.NodeSyncPeriod); err != nil {
		glog.Errorf("Failed to start service controller: %v", err)
	}

	resourceQuotaManager := resourcequota.NewResourceQuotaManager(kubeClient)
	resourceQuotaManager.Run(s.ResourceQuotaSyncPeriod)

	namespaceManager := namespace.NewNamespaceManager(kubeClient, s.NamespaceSyncPeriod)
	namespaceManager.Run()

	pvclaimBinder := volumeclaimbinder.NewPersistentVolumeClaimBinder(kubeClient, s.PVClaimBinderSyncPeriod)
	pvclaimBinder.Run()

	if len(s.ServiceAccountKeyFile) > 0 {
		privateKey, err := serviceaccount.ReadPrivateKey(s.ServiceAccountKeyFile)
		if err != nil {
			glog.Errorf("Error reading key for service account token controller: %v", err)
		} else {
			serviceaccount.NewTokensController(
				kubeClient,
				serviceaccount.DefaultTokenControllerOptions(
					serviceaccount.JWTTokenGenerator(privateKey),
				),
			).Run()
		}
	}

	serviceaccount.NewServiceAccountsController(
		kubeClient,
		serviceaccount.DefaultServiceAccountsControllerOptions(),
	).Run()

	select {}
	return nil
}
