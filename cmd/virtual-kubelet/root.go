package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"cloud.google.com/go/compute/metadata"

	"github.com/alexmwu/confidential-space-shim/pkg/provider"
	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"github.com/virtual-kubelet/virtual-kubelet/log"
	logruslogger "github.com/virtual-kubelet/virtual-kubelet/log/logrus"
	"github.com/virtual-kubelet/virtual-kubelet/node"
	"github.com/virtual-kubelet/virtual-kubelet/node/nodeutil"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

var (
	buildVersion = "0.0.1"
	k8sVersion   = "v1.25.0" // This should follow the version of k8s.io/kubernetes we are importing

	kubeConfigPath = envOrDefault("KUBECONFIG", "/usr/local/google/home/wuale/.kube/config")

	// Default resource capacity for CS VMs, based on default N2D quotas.
	defaultCPUCapacity     = "24"
	defaultMemoryCapacity  = "96Gi"  // (8GB per n2d-standard-2)*(12 n2d-standard-2 per region)
	defaultStorageCapacity = "132Gi" // 12*11GB
	defaultPodCapacity     = "12"

	taintKey    = envOrDefault("VKUBELET_TAINT_KEY", "virtual-kubelet.io/provider")
	taintEffect = envOrDefault("VKUBELET_TAINT_EFFECT", string(v1.TaintEffectNoSchedule))
	taintValue  = envOrDefault("VKUBELET_TAINT_VALUE", "confidential-space")

	certPath = os.Getenv("APISERVER_CERT_LOCATION")
	keyPath  = os.Getenv("APISERVER_KEY_LOCATION")

	nodeName       = "cs-vk-node"
	startupTimeout = 1 * time.Minute
	logLevel       = "info"
)

var (
	projectNumber  int64
	serviceAccount string
	projectID      string
	zone           string
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	binaryName := filepath.Base(os.Args[0])
	desc := binaryName + " implements a node on a Kubernetes cluster using Confidential Space to run pods."

	mdsClient := metadata.NewClient(nil)

	cmd := &cobra.Command{
		Use:   binaryName,
		Short: desc,
		Long:  desc,
		Run: func(cmd *cobra.Command, args []string) {
			logger := logrus.StandardLogger()
			fmt.Printf("Logging with level: %v\n", logLevel)
			lvl, err := logrus.ParseLevel(logLevel)
			if err != nil {
				logrus.WithError(err).Fatal("Error parsing log level")
			}
			logger.SetLevel(lvl)
			fmt.Println(kubeConfigPath)
			log.G(ctx).Debug(kubeConfigPath)

			ctx := log.WithLogger(ctx, logruslogger.FromLogrus(logrus.NewEntry(logger)))

			if err := run(ctx, mdsClient, logger); err != nil {
				if !errors.Is(err, context.Canceled) {
					log.G(ctx).Fatal(err)
				}
				log.G(ctx).Debug(err)
			}
		},
	}
	addProjectIDFlag(cmd)
	addZoneFlag(cmd)
	addLogFlag(cmd)
	addProjectNumberFlag(cmd)
	addServiceAccountFlag(cmd)

	if err := cmd.ExecuteContext(ctx); err != nil {
		if !errors.Is(err, context.Canceled) {
			logrus.WithError(err).Fatal("Error running command")
		}
	}
}

func run(ctx context.Context, mdsClient *metadata.Client, logger *logrus.Logger) error {
	var err error
	if projectID == "" {
		projectID, err = mdsClient.ProjectID()
		if err != nil {
			return fmt.Errorf("failed to retrieve projectID from MDS: %v", err)

		}
	}
	if zone == "" {
		zone, err = mdsClient.Zone()
		if err != nil {
			return fmt.Errorf("failed to retrieve zone from MDS: %v", err)
		}
	}

	if kubeConfigPath == "" {
		kubeConfigPath = "/var/lib/kubelet/kubeconfig"
	}
	k8sClient, err := nodeutil.ClientsetFromEnv(kubeConfigPath)
	if err != nil {
		log.G(ctx).Fatal(err)
	}
	withClient := func(cfg *nodeutil.NodeConfig) error {
		return nodeutil.WithClient(k8sClient)(cfg)
	}
	log.G(ctx).Debugf("Creating k8s Node with kubeconfig: %v", kubeConfigPath)
	node, err := nodeutil.NewNode(nodeName,
		func(cfg nodeutil.ProviderConfig) (nodeutil.Provider, node.NodeProvider, error) {
			p, err := provider.NewProvider(ctx, provider.ProviderOpts{
				Logger:         logger,
				Zone:           zone,
				ProjectID:      projectID,
				ProjectNumber:  projectNumber,
				ServiceAccount: serviceAccount,
			})
			if err != nil {
				return nil, nil, err
			}
			cfg.Node.Status.Capacity = capacity()
			cfg.Node.Status.Allocatable = capacity()
			return p, nil, err
		},
		withTaint,
		withVersion,
		withClient,
	)
	if err != nil {
		return err
	}

	go func() error {
		err = node.Run(ctx)
		if err != nil {
			return fmt.Errorf("error running the node: %w", err)
		}
		return nil
	}()

	if err := node.WaitReady(ctx, startupTimeout); err != nil {
		return fmt.Errorf("error waiting for node to be ready: %w", err)
	}

	<-node.Done()
	return node.Err()
}

func capacity() v1.ResourceList {
	return v1.ResourceList{
		v1.ResourceCPU:     resource.MustParse(defaultCPUCapacity),
		v1.ResourceMemory:  resource.MustParse(defaultMemoryCapacity),
		v1.ResourceStorage: resource.MustParse(defaultStorageCapacity),
		v1.ResourcePods:    resource.MustParse(defaultPodCapacity),
	}

}

func withTaint(cfg *nodeutil.NodeConfig) error {
	taint := v1.Taint{
		Key:    taintKey,
		Value:  taintValue,
		Effect: v1.TaintEffectNoSchedule,
	}
	cfg.NodeSpec.Spec.Taints = append(cfg.NodeSpec.Spec.Taints, taint)
	return nil
}

func withVersion(cfg *nodeutil.NodeConfig) error {
	cfg.NodeSpec.Status.NodeInfo.KubeletVersion = strings.Join([]string{k8sVersion, "confidential-space-vk", buildVersion}, "-")
	return nil
}

func envOrDefault(key string, defaultValue string) string {
	v, set := os.LookupEnv(key)
	if set {
		return v
	}
	return defaultValue
}

func addProjectIDFlag(cmd *cobra.Command) {
	cmd.PersistentFlags().StringVar(&projectID, "project-id", "", "GCP Project ID")
}

func addZoneFlag(cmd *cobra.Command) {
	cmd.PersistentFlags().StringVar(&zone, "zone", "", "GCP Zone to run CS-backed Kubernetes Pods")
}

func addLogFlag(cmd *cobra.Command) {
	cmd.PersistentFlags().StringVar(&logLevel, "log-level", logLevel, "log level, one of: trace, debug, info, warning, error, fatal, panic")
}

func addProjectNumberFlag(cmd *cobra.Command) {
	cmd.PersistentFlags().Int64Var(&projectNumber, "project-number", 0, "GCP Project Number: used to template GCE default SA. One of --service-account (takes precedence) or --project-number is required.")
}

func addServiceAccountFlag(cmd *cobra.Command) {
	cmd.PersistentFlags().StringVar(&serviceAccount, "service-account", "", "service account used to run the Confidential Space workload. One of --service-account (takes precedence) or --project-number is required.")
}
