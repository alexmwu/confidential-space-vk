// provider implements a new Virtual Kubelet Provider to create Confidential Spaces.
package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"strconv"

	"github.com/alexmwu/confidential-space-shim/pkg/confidentialspace"
	"github.com/googleapis/gax-go/v2/apierror"
	dto "github.com/prometheus/client_model/go"
	"github.com/sirupsen/logrus"
	"github.com/virtual-kubelet/virtual-kubelet/node/api"
	"github.com/virtual-kubelet/virtual-kubelet/node/api/statsv1alpha1"
	"github.com/virtual-kubelet/virtual-kubelet/trace"
	v1 "k8s.io/api/core/v1"
)

// Provider implements the virtual-kubelet provider interface and manages pods
// in a Confidential Space runtime.
type Provider struct {
	csClient *confidentialspace.Client
	logger   *logrus.Logger
}

type ProviderOpts struct {
	Logger         *logrus.Logger
	Zone           string
	ProjectID      string
	ProjectNumber  int64
	ServiceAccount string
}

// NewProvider creates a new Confidential Space Provider.
func NewProvider(ctx context.Context, opts ProviderOpts) (*Provider, error) {
	if opts.Zone == "" || opts.ProjectID == "" {
		return nil, fmt.Errorf("must provice zone (%v) and project ID (%v)", opts.Zone, opts.ProjectID)
	}
	csClient, err := confidentialspace.NewClient(ctx, opts.Zone, opts.ProjectID, opts.ProjectNumber, opts.ServiceAccount)
	if err != nil {
		return nil, fmt.Errorf("failed to create Confidential Space client: %v", err)
	}
	logger := opts.Logger
	if logger == nil {
		logger = logrus.StandardLogger()
	}
	return &Provider{
		csClient: csClient,
		logger:   logger,
	}, nil
}

// Create a Pod in Confidential Space.
func (p *Provider) CreatePod(ctx context.Context, pod *v1.Pod) error {
	ctx, span := trace.StartSpan(ctx, "confidentialspace.CreatePod")
	defer span.End()
	p.logger.Tracef("Called CreatePod: %+v", pod)
	instance, err := p.csClient.PodToInstance(pod)
	if err != nil {
		p.logger.Debugf("failed to create Instance object from Pod object: %v", err)
		return fmt.Errorf("failed to create Instance object from Pod object: %v", err)
	}
	op, err := p.csClient.Insert(ctx, instance)
	if err != nil {
		return err
	}

	err = op.Wait(ctx)
	if err != nil {
		p.logger.Debugf("failed to wait for GCE instance insert: %v", err)
		return fmt.Errorf("failed to wait for GCE instance insert: %w", err)
	}

	return nil
}

// UpdatePod is not required nor called by VK. Confidential Space also does not
// allow runtime container/pod updates.
func (p *Provider) UpdatePod(ctx context.Context, pod *v1.Pod) error {
	_, span := trace.StartSpan(ctx, "confidentialspace.UpdatePod")
	defer span.End()
	p.logger.Tracef("Called UpdatePod: %+v", pod)

	return nil
}

// DeletePod deletes the VM backing the Confidential Space pod.
func (p *Provider) DeletePod(ctx context.Context, pod *v1.Pod) error {
	_, span := trace.StartSpan(ctx, "confidentialspace.DeletePod")
	defer span.End()
	p.logger.Tracef("Called DeletePod: %+v", pod)

	op, err := p.csClient.Delete(ctx, pod)
	if err != nil {
		if e, ok := err.(*apierror.APIError); ok {
			if e.HTTPCode() == 404 {
				p.logger.Warnf("could not find GCE instance to delete: %v", err)
				return nil
			}
			p.logger.Infof("failed to delete GCE instance googleapi error (code %v): %v", e.HTTPCode(), e.Details())
		}
		p.logger.Debugf("failed to delete GCE instance: %v", err)
		return err
	}
	if err = op.Wait(ctx); err != nil {
		p.logger.Debugf("unable to wait for GCE delete operation: %v", err)
		return fmt.Errorf("unable to wait for GCE delete operation: %w", err)
	}

	return nil
}

// Provider function to return a Pod spec - mostly used for its status.
func (p *Provider) GetPod(ctx context.Context, namespace, name string) (*v1.Pod, error) {
	_, span := trace.StartSpan(ctx, "confidentialspace.GetPod")
	defer span.End()
	p.logger.Tracef("Called GetPod: %v", namespace+"/"+name)

	// TODO: add some caching
	instance, err := p.csClient.Get(ctx, namespace, name)
	if err != nil {
		p.logger.Debugf("failed to Get instance %v: %v", name, err)
		return nil, fmt.Errorf("failed to Get instance %v: %v", name, err)
	}

	inst, err := p.csClient.InstanceToPod(instance)
	if err != nil {
		p.logger.Debugf("failed to convert instance to Pod: %v", err)
		return nil, fmt.Errorf("failed to convert instance to Pod: %v", err)
	}
	p.logger.Tracef("GetPod response: %+v", inst)
	return inst, nil
}

func (p *Provider) GetPodStatus(ctx context.Context, namespace, name string) (*v1.PodStatus, error) {
	_, span := trace.StartSpan(ctx, "confidentialspace.GetPodStatus")
	defer span.End()
	p.logger.Tracef("Called GetPodStatus: %v", namespace+"/"+name)

	// TODO: speed up all the API calls and fetch only PodStatus
	pod, err := p.GetPod(ctx, namespace, name)
	if err != nil {
		p.logger.Debugf("failed to call GetPodStatus: %v", err)
		return nil, err
	}
	p.logger.Tracef("GetPodStatus response: %+v", &pod.Status)
	return &pod.Status, nil
}

// GetPods returns a list of all pods running on the Confidential Space
// provider.
func (p *Provider) GetPods(ctx context.Context) ([]*v1.Pod, error) {
	_, span := trace.StartSpan(ctx, "confidentialspace.GetPods")
	defer span.End()
	log.Print("Called GetPods")

	var wrappedErr error
	var pods []*v1.Pod
	instances, err := p.csClient.List(ctx)
	if err != nil {
		log.Printf("failed fetching all CS VK instances: %v", err)

		return nil, fmt.Errorf("failed fetching all CS VK instances: %v", err)
	}
	for _, instance := range instances {
		pod, err := p.csClient.InstanceToPod(instance)
		if err != nil {
			wrappedErr = errors.Join(wrappedErr, err)
		} else {
			pods = append(pods, pod)
		}
	}
	if wrappedErr != nil {
		p.logger.Debugf("failed to GetPods: %v", wrappedErr)
		return nil, fmt.Errorf("failed to GetPods: %v", wrappedErr)
	}

	p.logger.Tracef("GetPods response: %+v", pods)
	return pods, nil
}

// GetContainerLogs retrieves the logs of a container by name from the provider.
func (p *Provider) GetContainerLogs(ctx context.Context, namespace, podName, containerName string, opts api.ContainerLogOpts) (io.ReadCloser, error) {
	_, span := trace.StartSpan(ctx, "confidentialspace.GetContainerLogs")
	defer span.End()
	p.logger.Tracef("Called GetContainerLogs: %v", namespace+"/"+podName+"/"+containerName)

	// TODO: implement a way to get workload logs.
	return nil, nil
}

// RunInContainer is not implemented. Confidential Space does not allow
// runtime workload modifications.
func (p *Provider) RunInContainer(ctx context.Context, namespace, podName, containerName string, cmd []string, attach api.AttachIO) error {
	_, span := trace.StartSpan(ctx, "confidentialspace.RunInContainer")
	defer span.End()
	p.logger.Tracef("Called RunInContainer: %v", namespace+"/"+podName+"/"+containerName)

	return nil
}

// AttachToContainer is not implemented. Confidential Space does not allow
// runtime container attachments.
func (p *Provider) AttachToContainer(ctx context.Context, namespace, podName, containerName string, attach api.AttachIO) error {
	_, span := trace.StartSpan(ctx, "confidentialspace.AttachToContainer")
	defer span.End()
	p.logger.Tracef("Called AttachToContainer: %v", namespace+"/"+podName+"/"+containerName)

	return nil
}

// GetStatsSummary gets the stats for the node, including running pods
func (p *Provider) GetStatsSummary(ctx context.Context) (*statsv1alpha1.Summary, error) {
	_, span := trace.StartSpan(ctx, "confidentialspace.GetStatsSummary")
	defer span.End()
	p.logger.Tracef("Called GetStatsSummary")

	// TODO: implement a way to get CS VM stats.
	return nil, nil
}

// GetMetricsResource gets the metrics for the node, including running pods
func (p *Provider) GetMetricsResource(ctx context.Context) ([]*dto.MetricFamily, error) {
	_, span := trace.StartSpan(ctx, "confidentialspace.GetMetricsResource")
	defer span.End()
	p.logger.Tracef("Called GetMetricsResource")

	// TODO: implement a way to get CS metrics.
	return nil, nil
}

// PortForward is not implemented. Confidential Space does not support port
// forwarding.
func (p *Provider) PortForward(ctx context.Context, namespace, pod string, port int32, stream io.ReadWriteCloser) error {
	_, span := trace.StartSpan(ctx, "confidentialspace.PortForward")
	defer span.End()
	p.logger.Tracef("Called PortForward: %v", namespace+"/"+pod+"/"+strconv.FormatInt(int64(port), 10))

	return nil
}
