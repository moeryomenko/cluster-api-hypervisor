/*
Copyright 2026 The cluster-api-hypervisor Authors.

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

// Package main is the entry point for the cluster-api-hypervisor provider
// manager. It wires the scheme, the manager (webhook server, health probes,
// event broadcaster), the four controllers (the two infrastructure
// controllers plus the bootstrap and control-plane controllers), and the
// five admission webhooks, then runs until a shutdown signal arrives.
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"time"

	"github.com/spf13/pflag"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	cgrecord "k8s.io/client-go/tools/record"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	ctrlwebhook "sigs.k8s.io/controller-runtime/pkg/webhook"

	bootstrapv1alpha1 "github.com/moeryomenko/cluster-api-hypervisor/api/bootstrap/v1alpha1"
	controlplanev1alpha1 "github.com/moeryomenko/cluster-api-hypervisor/api/controlplane/v1alpha1"
	infrav1 "github.com/moeryomenko/cluster-api-hypervisor/api/v1alpha1"
	"github.com/moeryomenko/cluster-api-hypervisor/controllers"
	"github.com/moeryomenko/cluster-api-hypervisor/internal/chclient"
	"github.com/moeryomenko/cluster-api-hypervisor/internal/cloudinit"
	"github.com/moeryomenko/cluster-api-hypervisor/internal/confext"
	"github.com/moeryomenko/cluster-api-hypervisor/internal/confexttree"
	"github.com/moeryomenko/cluster-api-hypervisor/internal/config"
	"github.com/moeryomenko/cluster-api-hypervisor/internal/k8netd"
	"github.com/moeryomenko/cluster-api-hypervisor/internal/mac"
	"github.com/moeryomenko/cluster-api-hypervisor/internal/pki"
	providerwebhook "github.com/moeryomenko/cluster-api-hypervisor/internal/webhook"
	"github.com/moeryomenko/cluster-api-hypervisor/version"
)

const (
	// defaultWebhookPort is the port the webhook server binds to when
	// --webhook-port is not set, mirroring the kubebuilder/microvm default.
	defaultWebhookPort = 9443
	// defaultHealthAddr is the bind address of the health and readiness
	// endpoints when --health-addr is not set.
	defaultHealthAddr = ":9440"
	// defaultEventBurstSize is the event recorder burst size. Machine and
	// cluster operations can create enough events to trigger the event
	// recorder spam filter; a higher burst size ensures all events are
	// recorded and submitted to the API.
	defaultEventBurstSize = 100

	// controlPlaneRole is the node role the bootstrap data of a control-plane
	// Machine is rendered for.
	controlPlaneRole = "control-plane"

	// workloadAPIServerPort is the port the workload apiserver serves on and
	// the readiness healthz poller targets.
	workloadAPIServerPort = 6443

	// apiserverHealthzTimeout bounds one apiserver healthz request of the
	// control-plane readiness poller.
	apiserverHealthzTimeout = 10 * time.Second

	// defaultControlPlanePKIName is the fixed DNS SAN input of the cluster
	// PKI the control-plane controller generates on the first replica: the
	// control-plane role name. The IP SAN input is not pinned here — it is
	// the cp-0 internal IP reserved through k8netd AllocateIP before the PKI
	// is generated.
	defaultControlPlanePKIName = "control-plane"
)

var (
	// scheme is the runtime scheme shared by the manager, the clients, and
	// the webhook builders.
	scheme = runtime.NewScheme()
	// setupLog is the logger used by the startup path before the manager
	// owns the logging.
	setupLog = ctrl.Log.WithName("setup")

	kubeconfig                        string
	webhookCertDir                    string
	webhookPort                       int
	healthAddr                        string
	hypervisorClusterConcurrency      int
	hypervisorMachineConcurrency      int
	hypervisorConfigConcurrency       int
	hypervisorControlPlaneConcurrency int
)

// init registers the scheme: the client-go core types, the CAPI core types
// (v1beta1), and the three provider API groups. The manager needs all of them
// to watch and reconcile management-plane objects.
func init() {
	_ = clientgoscheme.AddToScheme(scheme)
	_ = clusterv1.AddToScheme(scheme)
	_ = infrav1.AddToScheme(scheme)
	_ = bootstrapv1alpha1.AddToScheme(scheme)
	_ = controlplanev1alpha1.AddToScheme(scheme)
}

// initFlags defines the manager command-line flags. The pinned contract flags
// are the kubeconfig path, the webhook certificate directory and port, the
// health probe address, and the per-controller concurrency placeholder.
func initFlags(fs *pflag.FlagSet) {
	fs.StringVar(
		&kubeconfig,
		"kubeconfig",
		"",
		"Path to the kubeconfig for the management cluster; empty uses the in-cluster config or the default loading rules.",
	)
	fs.StringVar(
		&webhookCertDir,
		"webhook-cert-dir",
		"/tmp/k8s-webhook-server/serving-certs",
		"The directory that contains the webhook server key and certificate (tls.key, tls.crt)",
	)
	fs.IntVar(
		&webhookPort,
		"webhook-port",
		defaultWebhookPort,
		"Webhook server port",
	)
	fs.StringVar(
		&healthAddr,
		"health-addr",
		defaultHealthAddr,
		"The address the health and readiness endpoints bind to.",
	)
	fs.IntVar(
		&hypervisorClusterConcurrency,
		"hypervisorcluster-concurrency",
		1,
		"Number of HypervisorClusters to process simultaneously",
	)
	fs.IntVar(
		&hypervisorMachineConcurrency,
		"hypervisormachine-concurrency",
		1,
		"Number of HypervisorMachines to process simultaneously",
	)
	fs.IntVar(
		&hypervisorConfigConcurrency,
		"hypervisorconfig-concurrency",
		1,
		"Number of HypervisorConfigs to process simultaneously",
	)
	fs.IntVar(
		&hypervisorControlPlaneConcurrency,
		"hypervisorcontrolplane-concurrency",
		1,
		"Number of HypervisorControlPlanes to process simultaneously",
	)
}

func main() {
	initFlags(pflag.CommandLine)
	pflag.Parse()

	restConfig, err := managerRestConfig()
	if err != nil {
		setupLog.Error(err, "unable to load the management cluster config")
		os.Exit(1)
	}

	// Resolve the provider configuration from the environment before any
	// controller is constructed: the infra controllers consume the host
	// paths and binaries it locates.
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		setupLog.Error(err, "unable to load the provider configuration")
		os.Exit(1)
	}

	// Machine and cluster operations can create enough events to trigger the
	// event recorder spam filter; setting the burst size higher ensures all
	// events will be recorded and submitted to the API.
	broadcaster := cgrecord.NewBroadcasterWithCorrelatorOptions(cgrecord.CorrelatorOptions{
		BurstSize: defaultEventBurstSize,
	})

	mgr, err := ctrl.NewManager(restConfig, ctrl.Options{
		Scheme: scheme,
		WebhookServer: ctrlwebhook.NewServer(ctrlwebhook.Options{
			Port:    webhookPort,
			CertDir: webhookCertDir,
		}),
		HealthProbeBindAddress: healthAddr,
		// Leader election is disabled: the management plane runs a single
		// provider quadlet (spec section 2.3), so there is no second
		// replica to arbitrate between.
		LeaderElection:   false,
		EventBroadcaster: broadcaster,
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	if err := setupWebhooks(mgr); err != nil {
		setupLog.Error(err, "failed to add webhooks")
		os.Exit(1)
	}

	if err := addHealthChecks(mgr); err != nil {
		setupLog.Error(err, "failed to add health checks")
		os.Exit(1)
	}

	if err := setupControllers(mgr, cfg); err != nil {
		setupLog.Error(err, "failed to add controllers")
		os.Exit(1)
	}

	setupLog.Info("starting manager",
		"version", version.Get().String(),
		"webhook-port", webhookPort,
		"health-addr", healthAddr,
		"hypervisorcluster-concurrency", hypervisorClusterConcurrency,
		"hypervisormachine-concurrency", hypervisorMachineConcurrency,
		"hypervisorconfig-concurrency", hypervisorConfigConcurrency,
		"hypervisorcontrolplane-concurrency", hypervisorControlPlaneConcurrency,
	)

	// Setup the context that is used for the manager and cancelled on a
	// shutdown signal (SIGTERM/SIGINT), giving the manager a graceful stop.
	ctx := ctrl.SetupSignalHandler()
	if err := mgr.Start(ctx); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}

// managerRestConfig returns the management cluster REST config: the
// --kubeconfig flag when set, otherwise the controller-runtime default
// loading rules (KUBECONFIG env, ~/.kube/config, or the in-cluster config
// when running inside the management plane).
func managerRestConfig() (*rest.Config, error) {
	if kubeconfig == "" {
		return ctrl.GetConfigOrDie(), nil
	}
	restConfig, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		return nil, fmt.Errorf("load kubeconfig %q: %w", kubeconfig, err)
	}
	return restConfig, nil
}

// setupWebhooks registers the five admission webhooks (three infrastructure
// kinds, one bootstrap kind, one control-plane kind) with the manager.
func setupWebhooks(mgr ctrl.Manager) error {
	if err := (&providerwebhook.HypervisorClusterWebhook{}).SetupWebhookWithManager(mgr); err != nil {
		return fmt.Errorf("unable to set up HypervisorCluster webhook: %w", err)
	}
	if err := (&providerwebhook.HypervisorMachineWebhook{}).SetupWebhookWithManager(mgr); err != nil {
		return fmt.Errorf("unable to set up HypervisorMachine webhook: %w", err)
	}
	if err := (&providerwebhook.HypervisorMachineTemplateWebhook{}).SetupWebhookWithManager(mgr); err != nil {
		return fmt.Errorf("unable to set up HypervisorMachineTemplate webhook: %w", err)
	}
	if err := (&providerwebhook.HypervisorConfigWebhook{}).SetupWebhookWithManager(mgr); err != nil {
		return fmt.Errorf("unable to set up HypervisorConfig webhook: %w", err)
	}
	if err := (&providerwebhook.HypervisorControlPlaneWebhook{}).SetupWebhookWithManager(mgr); err != nil {
		return fmt.Errorf("unable to set up HypervisorControlPlane webhook: %w", err)
	}
	return nil
}

// addHealthChecks registers the healthz and readyz endpoints on the health
// probe bind address.
func addHealthChecks(mgr ctrl.Manager) error {
	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		return fmt.Errorf("unable to add healthz check: %w", err)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		return fmt.Errorf("unable to add readyz check: %w", err)
	}
	return nil
}

// setupControllers constructs the four controllers with their host-side and
// PKI seams and registers them with the manager, each running at the
// concurrency of its flag. The HypervisorCluster controller owns the cluster
// network via k8netd; the HypervisorMachine controller drives one
// cloud-hypervisor VM per machine via k8netd ports; the HypervisorConfig
// controller renders the role-split bootstrap confext trees into the
// conventional data Secret; and the HypervisorControlPlane controller manages
// the control-plane Machine set and polls the workload apiserver for
// readiness.
func setupControllers(mgr ctrl.Manager, cfg config.Config) error {
	if err := (&controllers.HypervisorClusterReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Recorder: mgr.GetEventRecorderFor("hypervisorcluster-controller"),
		K8Netd:   k8netd.NewClient(cfg.K8NetdSocket),
	}).SetupWithManager(mgr, controller.Options{MaxConcurrentReconciles: hypervisorClusterConcurrency}); err != nil {
		return fmt.Errorf("unable to set up HypervisorCluster controller: %w", err)
	}

	if err := (&controllers.HypervisorMachineReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Recorder: mgr.GetEventRecorderFor("hypervisormachine-controller"),
		Config:   cfg,
		VM:       chclient.NewVMClient(cfg.SocketDir, cfg.CHBinary),
		K8Netd:   k8netd.NewClient(cfg.K8NetdSocket),
		QemuImg: func(ctx context.Context, name string, args ...string) ([]byte, error) {
			return exec.CommandContext(ctx, name, args...).CombinedOutput()
		},
		Confext:         confext.NewPackager(),
		RenderCloudInit: cloudinit.Render,
		DeriveMAC:       mac.Derive,
	}).SetupWithManager(mgr, controller.Options{MaxConcurrentReconciles: hypervisorMachineConcurrency}); err != nil {
		return fmt.Errorf("unable to set up HypervisorMachine controller: %w", err)
	}

	if err := (&controllers.HypervisorConfigReconciler{
		Client:              mgr.GetClient(),
		Scheme:              mgr.GetScheme(),
		Recorder:            mgr.GetEventRecorderFor("hypervisorconfig-controller"),
		BuildTree:           buildConfextTree,
		GenerateClusterPKI:  pki.GenerateClusterPKI,
		GenerateKubeletCert: pki.GenerateKubeletCert,
		RenderKubeconfig:    pki.RenderKubeconfig,
	}).SetupWithManager(mgr, controller.Options{MaxConcurrentReconciles: hypervisorConfigConcurrency}); err != nil {
		return fmt.Errorf("unable to set up HypervisorConfig controller: %w", err)
	}

	if err := (&controllers.HypervisorControlPlaneReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Recorder: mgr.GetEventRecorderFor("hypervisorcontrolplane-controller"),
		NewConfig: func(cp *controlplanev1alpha1.HypervisorControlPlane, machineName string) *bootstrapv1alpha1.HypervisorConfig {
			return &bootstrapv1alpha1.HypervisorConfig{
				ObjectMeta: metav1.ObjectMeta{
					Name:      machineName + "-config",
					Namespace: cp.Namespace,
				},
				Spec: bootstrapv1alpha1.HypervisorConfigSpec{
					Role: controlPlaneRole,
				},
			}
		},
		CreateMachine: func(ctx context.Context, machine *clusterv1.Machine) (client.Object, error) {
			if err := mgr.GetClient().Create(ctx, machine); err != nil {
				return nil, err
			}
			return machine, nil
		},
		GeneratePKI: func(cpIP string) (pki.ClusterPKI, error) {
			return pki.GenerateClusterPKI(cpIP, defaultControlPlanePKIName)
		},
		CheckAPIServerHealth: checkAPIServerHealth,
		K8Netd:               k8netd.NewClient(cfg.K8NetdSocket),
	}).SetupWithManager(mgr, controller.Options{MaxConcurrentReconciles: hypervisorControlPlaneConcurrency}); err != nil {
		return fmt.Errorf("unable to set up HypervisorControlPlane controller: %w", err)
	}

	return nil
}

// buildConfextTree renders the role-split confext tree for one node through
// the confexttree builders: the control-plane tree set (z-etcd,
// z-kubernetes-cp, z-kubelet-<node>) for a control-plane node, and the
// kubelet-only tree otherwise. The kubeconfigs map holds the rendered
// documents keyed by role ("kubelet", "admin", "controller-manager",
// "scheduler").
func buildConfextTree(
	role, cpIP, nodeName string,
	pk pki.ClusterPKI,
	kubeletCert, kubeletKey []byte,
	kubeconfigs map[string][]byte,
	encryptionConfig []byte,
) (map[string][]byte, error) {
	if role == controlPlaneRole {
		return confexttree.BuildControlPlane(
			cpIP, nodeName, pk, kubeletCert, kubeletKey,
			kubeconfigs["kubelet"],
			kubeconfigs["admin"],
			kubeconfigs["controller-manager"],
			kubeconfigs["scheduler"],
			encryptionConfig,
		)
	}
	return confexttree.BuildWorker(nodeName, pk, kubeletCert, kubeletKey, kubeconfigs["kubelet"])
}

// checkAPIServerHealth polls the workload apiserver healthz endpoint at
// https://cpIP:6443 with an HTTP client that trusts caCert and authenticates
// with the clientCert/clientKey pair, returning nil exactly when the apiserver
// answers 200 OK. The cluster CA doubles as the client certificate in the
// control-plane readiness flow, mirroring the KTHW apiserver client-ca-file
// authentication.
func checkAPIServerHealth(ctx context.Context, cpIP string, clientCert, clientKey, caCert []byte) error {
	caPool := x509.NewCertPool()
	if !caPool.AppendCertsFromPEM(caCert) {
		return fmt.Errorf("cluster CA bytes are not a PEM certificate")
	}
	cert, err := tls.X509KeyPair(clientCert, clientKey)
	if err != nil {
		return fmt.Errorf("build apiserver health client certificate: %w", err)
	}

	client := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				RootCAs:      caPool,
				Certificates: []tls.Certificate{cert},
				MinVersion:   tls.VersionTLS12,
			},
		},
		Timeout: apiserverHealthzTimeout,
	}

	url := "https://" + net.JoinHostPort(cpIP, strconv.Itoa(workloadAPIServerPort)) + "/healthz"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("build apiserver healthz request for %q: %w", url, err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() {
		_ = resp.Body.Close()
	}()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("apiserver healthz answered %s", resp.Status)
	}

	return nil
}
