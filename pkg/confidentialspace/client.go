package confidentialspace

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	compute "cloud.google.com/go/compute/apiv1"
	computepb "cloud.google.com/go/compute/apiv1/computepb"
	"github.com/google/go-tpm-tools/launcher/spec"
	"google.golang.org/protobuf/proto"
	v1 "k8s.io/api/core/v1"
)

type Client struct {
	projectID       string
	zone            string
	imagesClient    *compute.ImagesClient
	csImage         *computepb.Image
	instancesClient *compute.InstancesClient
}

// Copied from https://github.com/google/go-tpm-tools/blob/main/launcher/spec/launch_spec.go.
// We should export these in the future.
// Metadata variable names.
const (
	imageRefKey                = "tee-image-reference"
	signedImageRepos           = "tee-signed-image-repos"
	restartPolicyKey           = "tee-restart-policy"
	cmdKey                     = "tee-cmd"
	envKeyPrefix               = "tee-env-"
	impersonateServiceAccounts = "tee-impersonate-service-accounts"
	attestationServiceAddrKey  = "tee-attestation-service-endpoint"
	logRedirectKey             = "tee-container-log-redirect"
	memoryMonitoringEnable     = "tee-monitoring-memory-enable"
)

// Currently depends on Application Default Credentials.
func NewClient(ctx context.Context, zone string, project string) (*Client, error) {
	imagesClient, err := compute.NewImagesRESTClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to create compute images client: %w", err)
	}

	req := &computepb.GetFromFamilyImageRequest{
		Project: "confidential-space-images",
		Family:  "confidential-space",
	}
	image, err := imagesClient.GetFromFamily(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("failed to get latest Confidential Space image: %w", err)
	}

	instancesClient, err := compute.NewInstancesRESTClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to create compute instances client: %w", err)
	}
	defer instancesClient.Close()

	return &Client{
		projectID:       project,
		zone:            zone,
		imagesClient:    imagesClient,
		csImage:         image,
		instancesClient: instancesClient,
	}, nil
}

func (c *Client) Insert(ctx context.Context, instance *computepb.Instance) (*compute.Operation, error) {
	if !instance.GetConfidentialInstanceConfig().GetEnableConfidentialCompute() {
		return nil, errors.New("failed to create Confidential Space: confidential compute required")
	}
	// https://cloud.google.com/compute/docs/instances/create-start-instance#go
	req := &computepb.InsertInstanceRequest{
		Project:          c.projectID,
		Zone:             c.zone,
		InstanceResource: instance,
	}
	op, err := c.instancesClient.Insert(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("failed to insert GCE instance: %w", err)
	}

	err = op.Wait(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to wait for GCE instance insert: %w", err)
	}
	return op, nil
}

func (c *Client) GetLatestConfidentialSpace() *computepb.Image {
	return c.csImage
}

func (c *Client) podToInstance(pod *v1.Pod) (*computepb.Instance, error) {
	labels := make(map[string]string)
	labels["Namespace"] = pod.Namespace
	labels["UID"] = string(pod.UID)

	ls, err := podToLaunchSpec(pod)
	if err != nil {
		return nil, fmt.Errorf("failed to create LaunchSpec: %v", err)
	}
	md, err := launchSpecToCSMetadata(ls)
	if err != nil {
		return nil, fmt.Errorf("failed to convert LaunchSpec to Instance Metadata: %v", err)
	}
	instance := &computepb.Instance{
		Name:   &pod.Name,
		Labels: labels,
		Disks: []*computepb.AttachedDisk{
			{
				InitializeParams: &computepb.AttachedDiskInitializeParams{
					DiskSizeGb:  proto.Int64(11),
					DiskType:    proto.String(fmt.Sprintf("zones/%s/diskTypes/pd-standard", c.zone)),
					SourceImage: c.GetLatestConfidentialSpace().SelfLink,
				},
			},
		},
		MachineType: proto.String(fmt.Sprintf("zones/%s/machineTypes/n2d-standard-2", c.zone)),
		Metadata:    md,
		ConfidentialInstanceConfig: &computepb.ConfidentialInstanceConfig{
			EnableConfidentialCompute: proto.Bool(true),
		},
		ShieldedInstanceConfig: &computepb.ShieldedInstanceConfig{
			EnableIntegrityMonitoring: proto.Bool(true),
			EnableSecureBoot:          proto.Bool(true),
			EnableVtpm:                proto.Bool(true),
		},
	}
	return instance, nil
}

func podToLaunchSpec(pod *v1.Pod) (*spec.LaunchSpec, error) {
	// Other elements of Pod spec currently unsupported.
	if len(pod.Spec.Containers) != 1 {
		return nil, fmt.Errorf("only one container per VM is allowed in Confidential Space, got %d", len(pod.Spec.Containers))
	}
	ctr := pod.Spec.Containers[0]
	ls := &spec.LaunchSpec{
		ImageRef: ctr.Image,
		Cmd:      ctr.Args,
		Envs:     k8sEnvToLSEnv(ctr.Env),
	}
	// This may be problematic as the default RestartPolicy on CS is Never whereas on k8s it is Always.
	ls.RestartPolicy = spec.RestartPolicy(pod.Spec.RestartPolicy)
	return ls, nil

}

func k8sEnvToLSEnv(kevs []v1.EnvVar) []spec.EnvVar {
	lsev := make([]spec.EnvVar, 0, len(kevs))
	for _, kev := range kevs {
		lsev = append(lsev, spec.EnvVar{Name: kev.Name, Value: kev.Value})
	}
	return lsev
}

func launchSpecToCSMetadata(ls *spec.LaunchSpec) (*computepb.Metadata, error) {
	md := &computepb.Metadata{}
	md.Items = append(md.Items, &computepb.Items{Key: unaddressableToPtr(imageRefKey), Value: unaddressableToPtr(ls.ImageRef)})
	if len(ls.Cmd) != 0 {
		asJSON, err := json.Marshal(ls.Cmd)
		if err != nil {
			return nil, fmt.Errorf("failed to convert args to JSON: %w", err)
		}
		md.Items = append(md.Items, &computepb.Items{Key: unaddressableToPtr(cmdKey), Value: unaddressableToPtr(string(asJSON))})
	}
	md.Items = append(md.Items, &computepb.Items{Key: unaddressableToPtr(restartPolicyKey), Value: unaddressableToPtr((string)(ls.RestartPolicy))})
	for _, ev := range ls.Envs {
		if ev.Name == "" {
			return nil, fmt.Errorf("received an empty EnvVar key: value: %v", ev.Value)
		}
		md.Items = append(md.Items, &computepb.Items{Key: unaddressableToPtr(envKeyPrefix + ev.Name), Value: &ev.Value})
	}
	// TODO: implement log redirect

	return md, nil
}

func unaddressableToPtr(s string) *string {
	return &s
}

func (c *Client) Close() {
	c.imagesClient.Close()
	c.instancesClient.Close()
}
