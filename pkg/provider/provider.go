// confidentialspace implements a new Virtual Kubelet Provider to create Confidential Spaces.
package provider

import (
	"context"
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
	p.csClient.Insert()

	return nil
}

// UpdatePod is not required nor called by VK. Confidential Space also does not
// allow runtime container/pod updates.
func (p *Provider) UpdatePod(ctx context.Context, pod *v1.Pod) error {
	ctx, span := trace.StartSpan(ctx, "confidentialspace.UpdatePod")
	defer span.End()
	return nil
}

// Deletes the VM backing the Confidential Space pod.
func (p *Provider) DeletePod(ctx context.Context, pod *v1.Pod) error {
	ctx, span := trace.StartSpan(ctx, "confidentialspace.DeletePod")
	defer span.End()
	return nil
}

// Provider function to return a Pod spec - mostly used for its status
func (p *Provider) GetPod(ctx context.Context, namespace, name string) (*v1.Pod, error) {
	ctx, span := trace.StartSpan(ctx, "confidentialspace.GetPod")
	defer span.End()
	return &v1.Pod{}, nil
}

func (p *Provider) GetPodStatus(ctx context.Context, namespace, name string) (*v1.PodStatus, error) {
	ctx, span := trace.StartSpan(ctx, "confidentialspace.GetPodStatus")
	defer span.End()
	return &v1.PodStatus{}, nil
}

// GetPods returns a list of all pods running on the Confidential Space
// provider.
func (p *Provider) GetPods(ctx context.Context) ([]*v1.Pod, error) {
	ctx, span := trace.StartSpan(ctx, "confidentialspace.GetPods")
	defer span.End()
	return nil, nil
}
