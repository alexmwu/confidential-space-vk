# Confidential Space VK
Contains code to orchestrate Confidential Space VMs through Kubernetes via Virtual Kubelet.

# Usage
```
# Use ADC.
gcloud auth application-default login

# Set your Kubeconfig if you have one
KUBECONFIG="${HOME}/.kube/config"

# Or get creds for a GKE cluster:
gcloud container clusters get-credentials <gke-cluster-name> --location <location>
// e.g., gcloud container clusters get-credentials gke-standard-cluster-1-clone-1 --location us-west1

# Build VK from repo root:
CGO_ENABLED=0 go build ./cmd/virtual-kubelet

# Run VK:
./virtual-kubelet --zone <your zone> --project-id <your proj id> --project-number <your proj num>
# If zone/project are not specified, the CS provider will try to pull from MDS.

# For more verbose logging, pass
--log-level trace

# In another terminal:
kubectl get nodes
NAME                                                  STATUS   ROLES    AGE   VERSION                                                                             
cs-vk-node                                            Ready    agent    70m   v1.25.0-confidential-space-vk-0.0.1

kubectl describe node cs-vk-node
```

Create the following `pod.yaml` file:
```
apiVersion: v1
kind: Pod
metadata:
  name: cs-vk-pod
spec:
  nodeSelector:
    type: virtual-kubelet
  containers:
  - name: my-cs-container
    image: docker.io/library/nginx:latest
  tolerations:
  - key: "virtual-kubelet.io/provider"
    operator: "Equal"
    value: "confidential-space"
    effect: "NoSchedule" 

```


Create the pod:
```
kubectl create -f pod.yaml

kubectl get pods
NAME          READY   STATUS    RESTARTS   AGE
cs-pod-test   1/1     Running   0          5m36s


# If the pod fails scheduling:
kubectl describe pod cs-pod

# See the pod spin up:
gcloud compute instances list
```

Environment variables are not currently supported, due to K8S auto-populating
them and CS requiring env override in the launch spec:
```
Error log:
env var {KUBERNETES_SERVICE_PORT 443} is not allowed to be overridden on this image; allowed envs to be overridden: []

tee-env-KUBERNETES_SERVICE_PORT	
443
tee-env-KUBERNETES_SERVICE_PORT_HTTPS	
443
tee-env-KUBERNETES_PORT	
tcp://10.84.0.1:443
tee-env-KUBERNETES_PORT_443_TCP	
tcp://10.84.0.1:443
tee-env-KUBERNETES_PORT_443_TCP_PROTO	
tcp
tee-env-KUBERNETES_PORT_443_TCP_PORT	
443
tee-env-KUBERNETES_PORT_443_TCP_ADDR	
10.84.0.1
tee-env-KUBERNETES_SERVICE_HOST	
10.84.0.1
```