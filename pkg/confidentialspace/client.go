package confidentialspace

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	compute "cloud.google.com/go/compute/apiv1"
	computepb "cloud.google.com/go/compute/apiv1/computepb"
	"github.com/google/go-tpm-tools/launcher/spec"
	"google.golang.org/api/iterator"
	"google.golang.org/protobuf/proto"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

type Client struct {
	projectID       string
	zone            string
	imagesClient    *compute.ImagesClient
	csImage         *computepb.Image
	instancesClient *compute.InstancesClient
}

const (
	namespaceLabel = "Namespace"
	uidLabel       = "UID"
	vkTypeLabel    = "VKType"
	csType         = "ConfidentialSpace"
)

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

// Copied from https://github.com/google/go-tpm-tools/blob/main/launcher/spec/launch_spec.go.
// We should export this in the future.
func isValid(p spec.RestartPolicy) error {
	switch p {
	case spec.Always, spec.OnFailure, spec.Never:
		return nil
	}
	return fmt.Errorf("invalid restart policy: %s", p)
}

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

	return &Client{
		projectID:       project,
		zone:            zone,
		imagesClient:    imagesClient,
		csImage:         image,
		instancesClient: instancesClient,
	}, nil
}

// Insert takes an Instance object and inserts it into the configured projectID and zone.
// Callers should wait for the result of the Insert operation using (*compute.Operation).Wait.
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

	return op, nil
}

// Delete takes an instance name and deletes it with the configured projectID and zone.
// Callers should wait for the result of the Delete operation using (*compute.Operation).Wait.
func (c *Client) Delete(ctx context.Context, pod *v1.Pod) (*compute.Operation, error) {
	req := &computepb.DeleteInstanceRequest{
		Project:  c.projectID,
		Zone:     c.zone,
		Instance: c.InstanceName(pod),
	}

	op, err := c.instancesClient.Delete(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("failed to delete instance: %w", err)
	}

	return op, nil
}

func (c *Client) List(ctx context.Context) ([]*computepb.Instance, error) {
	var out []*computepb.Instance
	var wrappedErr error
	req := &computepb.AggregatedListInstancesRequest{
		Project: c.projectID,
		Filter:  unaddressableToPtr(fmt.Sprintf("(zone:%v)(labels.%v:%v)", c.zone, vkTypeLabel, csType)),
	}

	it := c.instancesClient.AggregatedList(ctx, req)
	for {
		pair, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			wrappedErr = errors.Join(wrappedErr, err)
		}
		instances := pair.Value.Instances
		if len(instances) > 0 {
			out = append(out, instances...)
		}
	}
	return out, nil

}

func (c *Client) Get(ctx context.Context, namespace, name string) (*computepb.Instance, error) {
	req := &computepb.GetInstanceRequest{
		Project:  c.projectID,
		Zone:     c.zone,
		Instance: namespace + name,
	}

	return c.instancesClient.Get(ctx, req)
}

func (c *Client) GetLatestConfidentialSpace() *computepb.Image {
	return c.csImage
}

// InstanceName returns the expected GCE instance name given a Pod.
func (c *Client) InstanceName(pod *v1.Pod) string {
	return pod.GetNamespace() + pod.GetName()
}

func (c *Client) PodToInstance(pod *v1.Pod) (*computepb.Instance, error) {
	labels := make(map[string]string)
	labels[namespaceLabel] = pod.GetNamespace()
	labels[uidLabel] = string(pod.GetUID())
	labels[vkTypeLabel] = csType

	ls, err := podToLaunchSpec(pod)
	if err != nil {
		return nil, fmt.Errorf("failed to create LaunchSpec: %v", err)
	}
	md, err := launchSpecToCSMetadata(ls)
	if err != nil {
		return nil, fmt.Errorf("failed to convert LaunchSpec to Instance Metadata: %v", err)
	}
	// TODO: parse ctr.Resources and convert to appropriate sized Instance MachineType.
	instance := &computepb.Instance{
		Name:   unaddressableToPtr(c.InstanceName(pod)),
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

func (c *Client) InstanceToPod(instance *computepb.Instance) (*v1.Pod, error) {
	// TODO: parse instance.ConfidentialInstanceConfig, instance.ShieldedInstanceConfig
	//	instance.Disks
	//	instance.Hostname
	//	instance.Id
	//	instance.MachineType
	//	instance.SourceMachineImage
	ls, err := csMetadataToLaunchSpec(instance.GetMetadata())
	if err != nil {
		return nil, fmt.Errorf("failed to convert Instance Metadata to LaunchSpec: %v", err)
	}
	ctr := v1.Container{
		Image: ls.ImageRef,
		Args:  ls.Cmd,
		Env:   lsEnvToK8sEnv(ls.Envs),
	}
	pod := &v1.Pod{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Pod",
			APIVersion: "v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: instance.GetName(),
		},
		Spec: v1.PodSpec{Containers: []v1.Container{ctr}},
	}
	pod.Spec.Containers = append(pod.Spec.Containers, ctr)
	labels := instance.GetLabels()
	if val, ok := labels[vkTypeLabel]; !(ok && val == csType) {
		log.Printf("missing VK label on VK-managed VM: %v", instance.GetName())
	}
	v1Time, err := instanceCreationTimeToV1(instance)
	if err != nil {
		return nil, err
	}
	pod.ObjectMeta.CreationTimestamp = v1Time

	ns, ok := labels[namespaceLabel]
	if !ok {
		log.Printf("failed to find namespace label for instance %v", instance.GetName())
	}
	pod.ObjectMeta.Namespace = ns
	uid, ok := labels[uidLabel]
	if !ok {
		log.Printf("failed to find UID label for instance %v", instance.GetName())
	}
	pod.ObjectMeta.UID = types.UID(uid)
	podStatus, err := getPodStatus(instance, ls)
	if err != nil {
		return nil, fmt.Errorf("failed to get PodStatus: %v", err)
	}
	pod.Status = *podStatus
	return pod, nil
}

func instanceCreationTimeToV1(instance *computepb.Instance) (metav1.Time, error) {
	createTime, err := time.Parse(time.RFC3339, instance.GetCreationTimestamp())
	if err != nil {
		return metav1.Time{}, fmt.Errorf("failed to parse Instance CreationTimestamp: %v", instance.GetCreationTimestamp())
	}
	return metav1.NewTime(createTime), nil
}

func instanceStopTimeToV1(instance *computepb.Instance) (metav1.Time, error) {
	createTime, err := time.Parse(time.RFC3339, instance.GetLastStopTimestamp())
	if err != nil {
		return metav1.Time{}, fmt.Errorf("failed to parse Instance LastStopTimestamp: %v", instance.GetLastStopTimestamp())
	}
	return metav1.NewTime(createTime), nil
}

func getPodStatus(instance *computepb.Instance, ls *spec.LaunchSpec) (*v1.PodStatus, error) {
	iStatus := instance.GetStatus()
	phase := v1.PodUnknown
	var ctrState v1.ContainerState
	// PodReady means the pod is able to service requests.
	isReady := v1.ConditionFalse
	// PodScheduled represents status of the scheduling process for this pod.
	isScheduled := v1.ConditionFalse
	v1Time, err := instanceCreationTimeToV1(instance)
	if err != nil {
		return nil, err
	}

	// Paraphrased from K8S docs:
	// Pending: Pod accepted by Kubernetes, but one or more of the container images has not been created.
	// Running: The pod bound to a node, and all of the containers have been created. >=1 container is still running, or (re)starting.
	// Succeeded: All containers in pod terminated in success, and will not be restarted.
	// Failed: All containers in pod terminated, and >=1 container terminated in failure (either exited with non-zero or terminated
	// by the system).
	// Unknown: For some reason the state of the pod could not be obtained, typically due to an
	// error in communicating with the host of the pod.
	// Paraphrased from GCE docs:
	// Instance_UNDEFINED_STATUS:  enum field is not set.
	// Instance_DEPROVISIONING: instance halted and GCE performing tear down tasks.
	// Instance_PROVISIONING: Resources are being allocated for the instance.
	// Instance_REPAIRING: The instance is in repair.
	// Instance_RUNNING: The instance is running.
	// Instance_STAGING: All required resources have been allocated and the instance is being started.
	// Instance_STOPPED: The instance has stopped successfully.
	// Instance_STOPPING: The instance is currently stopping (either being deleted or killed).
	// Instance_SUSPENDED: The instance has suspended.
	// Instance_SUSPENDING: The instance is suspending.
	// Instance_TERMINATED: The instance has stopped (either by explicit action or underlying failure).

	switch iStatus {
	// TODO: Get better definitions from metrics.
	case computepb.Instance_PROVISIONING.String(), computepb.Instance_STAGING.String(), computepb.Instance_SUSPENDED.String(), computepb.Instance_SUSPENDING.String():
		phase = v1.PodPending
		isScheduled = v1.ConditionTrue
		ctrState = v1.ContainerState{Waiting: &v1.ContainerStateWaiting{Reason: "Instance Status: " + iStatus}}
	case computepb.Instance_RUNNING.String():
		phase = v1.PodRunning
		isScheduled = v1.ConditionTrue
		isReady = v1.ConditionTrue
		ctrState = v1.ContainerState{Running: &v1.ContainerStateRunning{StartedAt: v1Time}}
	case computepb.Instance_STOPPED.String(), computepb.Instance_STOPPING.String(), computepb.Instance_TERMINATED.String():
		// TODO: get application-level metrics for container launcher exit code.
		phase = v1.PodSucceeded
		stop, err := instanceStopTimeToV1(instance)
		if err != nil {
			return nil, err
		}
		ctrState = v1.ContainerState{Terminated: &v1.ContainerStateTerminated{
			ExitCode:   0,
			Reason:     "Instance Status: " + iStatus,
			StartedAt:  v1Time,
			FinishedAt: stop}}
	case computepb.Instance_UNDEFINED_STATUS.String(), computepb.Instance_DEPROVISIONING.String(), computepb.Instance_REPAIRING.String():
		fallthrough
	default:
		phase = v1.PodUnknown
	}

	conditions := []v1.PodCondition{
		{
			Type:   v1.PodScheduled,
			Status: isScheduled,
		},
		{
			Type:   v1.PodReady,
			Status: isReady,
		},
	}
	nis := instance.GetNetworkInterfaces()
	ipv4Addr := ""
	for _, ni := range nis {
		if ni.GetName() == "nic0" {
			ipv4Addr = ni.GetNetworkIP()
		}
	}

	ctrRdy := false
	if isReady == v1.ConditionTrue {
		ctrRdy = true
	}

	ctrStatus := v1.ContainerStatus{
		State: ctrState,
		Ready: ctrRdy,
		Image: ls.ImageRef,
	}

	podStatus := &v1.PodStatus{
		Phase:             phase,
		Conditions:        conditions,
		ContainerStatuses: []v1.ContainerStatus{ctrStatus},
		HostIP:            ipv4Addr,
		PodIP:             ipv4Addr,
		QOSClass:          v1.PodQOSBestEffort,
	}

	return podStatus, nil
}

func podToLaunchSpec(pod *v1.Pod) (*spec.LaunchSpec, error) {
	// Other elements of Pod spec currently unsupported.
	if len(pod.Spec.Containers) != 1 {
		return nil, fmt.Errorf("only one container per VM is allowed in Confidential Space, got %d", len(pod.Spec.Containers))
	}
	ctr := pod.Spec.Containers[0]
	if len(ctr.Command) != 0 {
		return nil, fmt.Errorf("cannot override the entrypoint in Confidential Space, got %v", ctr.Command)
	}
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

func lsEnvToK8sEnv(levs []spec.EnvVar) []v1.EnvVar {
	k8sev := make([]v1.EnvVar, 0, len(levs))
	for _, lev := range levs {
		k8sev = append(k8sev, v1.EnvVar{Name: lev.Name, Value: lev.Value})
	}
	return k8sev
}

func csMetadataToLaunchSpec(md *computepb.Metadata) (*spec.LaunchSpec, error) {
	ls := &spec.LaunchSpec{}
	for _, item := range md.Items {
		k := *item.Key
		v := *item.Value
		if strings.HasPrefix(k, envKeyPrefix) {
			ls.Envs = append(ls.Envs, spec.EnvVar{Name: strings.TrimPrefix(k, envKeyPrefix), Value: v})
			continue
		}
		switch k {
		case imageRefKey:
			ls.ImageRef = v
		case restartPolicyKey:
			ls.RestartPolicy = spec.RestartPolicy(v)
			if err := isValid(ls.RestartPolicy); err != nil {
				return nil, err
			}
		case cmdKey:
			if err := json.Unmarshal([]byte(v), &ls.Cmd); err != nil {
				return nil, fmt.Errorf("failed to Unmarshal %v as JSON: %w", err)
			}
		default:
			log.Printf("got unknown metadata: %v", item)
		}
	}

	return ls, nil
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
