// confidentialspace implements a new Virtual Kubelet Provider to create Confidential Spaces.
package provider

import (
	"context"
	"errors"
	"fmt"

	"github.com/alexmwu/confidential-space-shim/pkg/confidentialspace"
	"github.com/virtual-kubelet/virtual-kubelet/trace"
	v1 "k8s.io/api/core/v1"
)

// Provider implements the virtual-kubelet provider interface and manages pods
// in a Confidential Space runtime.
type Provider struct {
	csClient *confidentialspace.Client
}

// NewProvider creates a new Confidential Space Provider.
func NewProvider(ctx context.Context, zone string, projectID string) (*Provider, error) {
	csClient, err := confidentialspace.NewClient(ctx, zone, projectID)
	if err != nil {
		return nil, fmt.Errorf("failed to create Confidential Space client: %v", err)
	}
	return &Provider{
		csClient: csClient,
	}, nil
}

// Create a Pod in Confidential Space.
func (p *Provider) CreatePod(ctx context.Context, pod *v1.Pod) error {
	ctx, span := trace.StartSpan(ctx, "confidentialspace.CreatePod")
	defer span.End()
	instance, err := p.csClient.PodToInstance(pod)
	if err != nil {
		return fmt.Errorf("failed to create Instance object from Pod object: %v", err)
	}
	op, err := p.csClient.Insert(ctx, instance)
	if err != nil {
		return err
	}

	err = op.Wait(ctx)
	if err != nil {
		return fmt.Errorf("failed to wait for GCE instance insert: %w", err)
	}

	return nil
}

// UpdatePod is not required nor called by VK. Confidential Space also does not
// allow runtime container/pod updates.
func (p *Provider) UpdatePod(ctx context.Context, pod *v1.Pod) error {
	_, span := trace.StartSpan(ctx, "confidentialspace.UpdatePod")
	defer span.End()
	return nil
}

// DeletePod deletes the VM backing the Confidential Space pod.
func (p *Provider) DeletePod(ctx context.Context, pod *v1.Pod) error {
	_, span := trace.StartSpan(ctx, "confidentialspace.DeletePod")
	defer span.End()
	op, err := p.csClient.Delete(ctx, pod)
	if err != nil {
		return err
	}
	if err = op.Wait(ctx); err != nil {
		return fmt.Errorf("unable to wait for the operation: %w", err)
	}

	return nil
}

// Provider function to return a Pod spec - mostly used for its status.
func (p *Provider) GetPod(ctx context.Context, namespace, name string) (*v1.Pod, error) {
	_, span := trace.StartSpan(ctx, "confidentialspace.GetPod")
	defer span.End()
	// TODO: add some caching
	instance, err := p.csClient.Get(ctx, namespace, name)
	if err != nil {
		return nil, fmt.Errorf("failed to Get instance %v", name)
	}
	return p.csClient.InstanceToPod(instance)
}

func (p *Provider) GetPodStatus(ctx context.Context, namespace, name string) (*v1.PodStatus, error) {
	_, span := trace.StartSpan(ctx, "confidentialspace.GetPodStatus")
	defer span.End()
	// TODO: speed up all the API calls and fetch only PodStatus
	pod, err := p.GetPod(ctx, namespace, name)
	if err != nil {
		return nil, err
	}
	return &pod.Status, nil
}

// GetPods returns a list of all pods running on the Confidential Space
// provider.
func (p *Provider) GetPods(ctx context.Context) ([]*v1.Pod, error) {
	_, span := trace.StartSpan(ctx, "confidentialspace.GetPods")
	defer span.End()
	var wrappedErr error
	var pods []*v1.Pod
	instances, err := p.csClient.List(ctx)
	if err != nil {
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
	return pods, wrappedErr
}
