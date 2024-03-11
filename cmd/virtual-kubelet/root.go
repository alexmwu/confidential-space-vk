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
)

var (
	buildVersion = "N/A"
	buildTime    = "N/A"
	k8sVersion   = "v1.25.0" // This should follow the version of k8s.io/kubernetes we are importing

	taintKey    = envOrDefault("VKUBELET_TAINT_KEY", "virtual-kubelet.io/provider")
	taintEffect = envOrDefault("VKUBELET_TAINT_EFFECT", string(v1.TaintEffectNoSchedule))
	taintValue  = envOrDefault("VKUBELET_TAINT_VALUE", "confidential-space")

	certPath = os.Getenv("APISERVER_CERT_LOCATION")
	keyPath  = os.Getenv("APISERVER_KEY_LOCATION")

	nodeName       = "cs-vk-node"
	startupTimeout = time.Minute
	logLevel       = "info"
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
			lvl, err := logrus.ParseLevel(logLevel)
			if err != nil {
				logrus.WithError(err).Fatal("Error parsing log level")
			}
			logger.SetLevel(lvl)

			ctx := log.WithLogger(ctx, logruslogger.FromLogrus(logrus.NewEntry(logger)))

			if err := run(ctx, mdsClient); err != nil {
				if !errors.Is(err, context.Canceled) {
					log.G(ctx).Fatal(err)
				}
				log.G(ctx).Debug(err)
			}
		},
	}

	if err := cmd.ExecuteContext(ctx); err != nil {
		if !errors.Is(err, context.Canceled) {
			logrus.WithError(err).Fatal("Error running command")
		}
	}
}

func run(ctx context.Context, mdsClient *metadata.Client) error {

	projectID, err := mdsClient.ProjectID()
	if err != nil {
		return fmt.Errorf("failed to retrieve projectID from MDS: %v", err)

	}
	zone, err := mdsClient.Zone()
	if err != nil {
		return fmt.Errorf("failed to retrieve zone from MDS: %v", err)
	}

	node, err := nodeutil.NewNode(nodeName,
		func(cfg nodeutil.ProviderConfig) (nodeutil.Provider, node.NodeProvider, error) {
			p, err := provider.NewProvider(ctx, zone, projectID)
			if err != nil {
				return nil, nil, err
			}
			return p, nil, err
		},
		withTaint,
		withVersion,
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
	cfg.NodeSpec.Status.NodeInfo.KubeletVersion = strings.Join([]string{k8sVersion, "vk-confidential-space", buildVersion}, "-")
	return nil
}

func envOrDefault(key string, defaultValue string) string {
	v, set := os.LookupEnv(key)
	if set {
		return v
	}
	return defaultValue
}
