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
// event broadcaster), and the five admission webhooks, then runs until a
// shutdown signal arrives.
package main

import (
	"fmt"
	"os"

	"github.com/spf13/pflag"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	cgrecord "k8s.io/client-go/tools/record"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	ctrlwebhook "sigs.k8s.io/controller-runtime/pkg/webhook"

	bootstrapv1alpha1 "github.com/moeryomenko/cluster-api-hypervisor/api/bootstrap/v1alpha1"
	controlplanev1alpha1 "github.com/moeryomenko/cluster-api-hypervisor/api/controlplane/v1alpha1"
	infrav1 "github.com/moeryomenko/cluster-api-hypervisor/api/v1alpha1"
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
)

var (
	// scheme is the runtime scheme shared by the manager, the clients, and
	// the webhook builders.
	scheme = runtime.NewScheme()
	// setupLog is the logger used by the startup path before the manager
	// owns the logging.
	setupLog = ctrl.Log.WithName("setup")

	kubeconfig                   string
	webhookCertDir               string
	webhookPort                  int
	healthAddr                   string
	hypervisorClusterConcurrency int
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
}

func main() {
	initFlags(pflag.CommandLine)
	pflag.Parse()

	restConfig, err := managerRestConfig()
	if err != nil {
		setupLog.Error(err, "unable to load the management cluster config")
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

	setupLog.Info("starting manager",
		"version", version.Get().String(),
		"webhook-port", webhookPort,
		"health-addr", healthAddr,
		"hypervisorcluster-concurrency", hypervisorClusterConcurrency,
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
