// Package kube reads cluster state through kubectl, always with the sandbox's own kubeconfig.
package kube

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/egezamb/helmbulance/internal/execx"
)

// Client reads one sandbox cluster.
type Client struct {
	Runner     *execx.Runner
	Kubeconfig string
}

// Secret is the data of one Secret. Values stay in memory and are never written anywhere.
type Secret struct {
	Name string
	Data map[string][]byte
}

// skippedSecretTypes are Secrets that change for reasons unrelated to the chart.
var skippedSecretTypes = map[string]bool{
	"helm.sh/release.v1":                  true, // Helm's own release record, rewritten on every upgrade
	"kubernetes.io/service-account-token": true,
}

// Secrets returns the Secrets in ns, sorted by name, without Helm's release records and service
// account tokens. It does not know which ones the chart renders; see helm.Client.OwnedSecrets.
func (c *Client) Secrets(ctx context.Context, ns string) ([]Secret, error) {
	out, err := c.Runner.Run(ctx, "kubectl", "--kubeconfig", c.Kubeconfig, "--namespace", ns, "get", "secrets", "-o", "json")
	if err != nil {
		return nil, err
	}
	var list struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Type string            `json:"type"`
			Data map[string]string `json:"data"`
		} `json:"items"`
	}
	if err := json.Unmarshal(out, &list); err != nil {
		return nil, fmt.Errorf("parse secrets: %w", err)
	}
	var secrets []Secret
	for _, item := range list.Items {
		if skippedSecretTypes[item.Type] {
			continue
		}
		s := Secret{Name: item.Metadata.Name, Data: map[string][]byte{}}
		for k, v := range item.Data {
			b, err := base64.StdEncoding.DecodeString(v)
			if err != nil {
				return nil, fmt.Errorf("secret %s key %s: %w", s.Name, k, err)
			}
			s.Data[k] = b
		}
		secrets = append(secrets, s)
	}
	sort.Slice(secrets, func(i, j int) bool { return secrets[i].Name < secrets[j].Name })
	return secrets, nil
}

// SecretConsumers maps "secret/key" to the workloads (kind/name) whose pod template reads it
// through env, envFrom or a volume. Used to explain the impact of a changed key.
func (c *Client) SecretConsumers(ctx context.Context, ns string) (map[string][]string, error) {
	out, err := c.Runner.Run(ctx, "kubectl", "--kubeconfig", c.Kubeconfig, "--namespace", ns,
		"get", "deployments,statefulsets,daemonsets", "-o", "json")
	if err != nil {
		return nil, err
	}
	var list struct {
		Items []struct {
			Kind     string `json:"kind"`
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Spec struct {
				Template struct {
					Spec podSpec `json:"spec"`
				} `json:"template"`
			} `json:"spec"`
		} `json:"items"`
	}
	if err := json.Unmarshal(out, &list); err != nil {
		return nil, fmt.Errorf("parse workloads: %w", err)
	}
	consumers := map[string][]string{}
	add := func(ref, workload string) {
		for _, w := range consumers[ref] {
			if w == workload {
				return
			}
		}
		consumers[ref] = append(consumers[ref], workload)
	}
	for _, item := range list.Items {
		workload := item.Kind + "/" + item.Metadata.Name
		spec := item.Spec.Template.Spec
		for _, ctr := range append(spec.InitContainers, spec.Containers...) {
			for _, e := range ctr.Env {
				if ref := e.ValueFrom.SecretKeyRef; ref.Name != "" {
					add(ref.Name+"/"+ref.Key, workload)
				}
			}
			for _, e := range ctr.EnvFrom {
				if e.SecretRef.Name != "" {
					add(e.SecretRef.Name+"/*", workload)
				}
			}
		}
		for _, v := range spec.Volumes {
			if v.Secret.SecretName != "" {
				add(v.Secret.SecretName+"/*", workload)
			}
		}
	}
	return consumers, nil
}

type podSpec struct {
	InitContainers []container `json:"initContainers"`
	Containers     []container `json:"containers"`
	Volumes        []struct {
		Secret struct {
			SecretName string `json:"secretName"`
		} `json:"secret"`
	} `json:"volumes"`
}

type container struct {
	Env []struct {
		ValueFrom struct {
			SecretKeyRef struct {
				Name string `json:"name"`
				Key  string `json:"key"`
			} `json:"secretKeyRef"`
		} `json:"valueFrom"`
	} `json:"env"`
	EnvFrom []struct {
		SecretRef struct {
			Name string `json:"name"`
		} `json:"secretRef"`
	} `json:"envFrom"`
}

// DeleteNamespace removes ns and waits until it is gone.
func (c *Client) DeleteNamespace(ctx context.Context, ns string) error {
	_, err := c.Runner.Run(ctx, "kubectl", "--kubeconfig", c.Kubeconfig, "delete", "namespace", ns,
		"--ignore-not-found", "--wait=true", "--timeout=5m")
	return err
}

// Pod is the health of one pod.
type Pod struct {
	Name     string
	UID      string
	Owner    string // kind/name of the controlling object
	OwnerUID string // its UID, which changes when the object is deleted and created again
	Ready    bool
	Restarts int
	Phase    string // Pending, Running or Failed; Succeeded pods are left out
	// Revision is the pod template revision of a Deployment (pod-template-hash) or a StatefulSet
	// or DaemonSet (controller-revision-hash); "" for other pods.
	Revision string
}

// FromJob reports whether p is an attempt of a Job. Such a pod ends by design, and a failed
// attempt may be retried, so its readiness says nothing about the app; the Job's own status does.
func (p Pod) FromJob() bool { return strings.HasPrefix(p.Owner, "Job/") }

// FinishedJob reports whether p is a Job pod that has finished, so its outcome belongs to the Job's
// status, not to readiness. A Job pod still Pending (stuck in ErrImagePull) or Running (crash-looping
// in place) has not finished: its Job stays active with no failure recorded, so it is still judged
// by readiness and restarts. Succeeded pods are already dropped from the pod list.
func (p Pod) FinishedJob() bool {
	return p.FromJob() && (p.Phase == "Succeeded" || p.Phase == "Failed")
}

// Pods returns the pods in ns with their readiness and restart counts. Finished pods and pods that
// are terminating are left out.
func (c *Client) Pods(ctx context.Context, ns string) ([]Pod, error) {
	out, err := c.Runner.Run(ctx, "kubectl", "--kubeconfig", c.Kubeconfig, "--namespace", ns, "get", "pods", "-o", "json")
	if err != nil {
		return nil, err
	}
	var list struct {
		Items []struct {
			Metadata struct {
				Name              string            `json:"name"`
				UID               string            `json:"uid"`
				DeletionTimestamp *string           `json:"deletionTimestamp"`
				Labels            map[string]string `json:"labels"`
				OwnerReferences   []struct {
					Kind string `json:"kind"`
					Name string `json:"name"`
					UID  string `json:"uid"`
				} `json:"ownerReferences"`
			} `json:"metadata"`
			Status struct {
				Phase      string `json:"phase"`
				Conditions []struct {
					Type   string `json:"type"`
					Status string `json:"status"`
				} `json:"conditions"`
				ContainerStatuses []struct {
					RestartCount int `json:"restartCount"`
				} `json:"containerStatuses"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal(out, &list); err != nil {
		return nil, fmt.Errorf("parse pods: %w", err)
	}
	var pods []Pod
	for _, item := range list.Items {
		if item.Status.Phase == "Succeeded" { // finished Jobs
			continue
		}
		if item.Metadata.DeletionTimestamp != nil { // on its way out, already replaced
			continue
		}
		p := Pod{Name: item.Metadata.Name, UID: item.Metadata.UID, Phase: item.Status.Phase}
		p.Revision = item.Metadata.Labels["pod-template-hash"]
		if p.Revision == "" {
			p.Revision = item.Metadata.Labels["controller-revision-hash"]
		}
		if refs := item.Metadata.OwnerReferences; len(refs) > 0 {
			p.Owner = refs[0].Kind + "/" + refs[0].Name
			p.OwnerUID = refs[0].UID
		}
		for _, c := range item.Status.Conditions {
			if c.Type == "Ready" && c.Status == "True" {
				p.Ready = true
			}
		}
		for _, cs := range item.Status.ContainerStatuses {
			p.Restarts += cs.RestartCount
		}
		pods = append(pods, p)
	}
	return pods, nil
}

// FailedJobs returns the Jobs in ns that gave up (condition Failed) or are retrying after a failed
// attempt, as "Job/<name>: <message>".
func (c *Client) FailedJobs(ctx context.Context, ns string) ([]string, error) {
	out, err := c.Runner.Run(ctx, "kubectl", "--kubeconfig", c.Kubeconfig, "--namespace", ns, "get", "jobs", "-o", "json")
	if err != nil {
		return nil, err
	}
	var list struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Status struct {
				Failed     int `json:"failed"`
				Conditions []struct {
					Type    string `json:"type"`
					Status  string `json:"status"`
					Reason  string `json:"reason"`
					Message string `json:"message"`
				} `json:"conditions"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal(out, &list); err != nil {
		return nil, fmt.Errorf("parse jobs: %w", err)
	}
	var failed []string
	for _, item := range list.Items {
		finished := false
		for _, cond := range item.Status.Conditions {
			if cond.Status != "True" {
				continue
			}
			switch cond.Type {
			case "Failed":
				failed = append(failed, "Job/"+item.Metadata.Name+": "+strings.TrimSpace(cond.Reason+" "+cond.Message))
				finished = true
			case "Complete":
				finished = true
			}
		}
		// The Failed condition comes only after the whole back-off, about ten minutes with the
		// default backoffLimit; a Job still retrying after a failed attempt counts too.
		if !finished && item.Status.Failed > 0 {
			failed = append(failed, fmt.Sprintf("Job/%s: %d failed attempt(s), still retrying", item.Metadata.Name, item.Status.Failed))
		}
	}
	sort.Strings(failed)
	return failed, nil
}

// Service is one Service of the release and the ports it exposes.
type Service struct {
	Name     string
	Selector map[string]string
	Ports    []ServicePort
	// Type is ClusterIP, NodePort, LoadBalancer or ExternalName. ClusterIP is "None" for a
	// headless Service.
	Type        string
	ClusterIP   string
	ExternalIPs []string
}

// ServicePort is one port of a Service.
type ServicePort struct {
	Name     string
	Port     int
	Protocol string
}

// Services returns the Services in ns that select pods (headless Services included).
func (c *Client) Services(ctx context.Context, ns string) ([]Service, error) {
	out, err := c.Runner.Run(ctx, "kubectl", "--kubeconfig", c.Kubeconfig, "--namespace", ns, "get", "services", "-o", "json")
	if err != nil {
		return nil, err
	}
	var list struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Spec struct {
				Type        string            `json:"type"`
				ClusterIP   string            `json:"clusterIP"`
				ExternalIPs []string          `json:"externalIPs"`
				Selector    map[string]string `json:"selector"`
				Ports       []struct {
					Name     string `json:"name"`
					Port     int    `json:"port"`
					Protocol string `json:"protocol"`
				} `json:"ports"`
			} `json:"spec"`
		} `json:"items"`
	}
	if err := json.Unmarshal(out, &list); err != nil {
		return nil, fmt.Errorf("parse services: %w", err)
	}
	var services []Service
	for _, item := range list.Items {
		if len(item.Spec.Selector) == 0 {
			continue
		}
		s := Service{
			Name:        item.Metadata.Name,
			Selector:    item.Spec.Selector,
			Type:        item.Spec.Type,
			ClusterIP:   item.Spec.ClusterIP,
			ExternalIPs: item.Spec.ExternalIPs,
		}
		for _, p := range item.Spec.Ports {
			s.Ports = append(s.Ports, ServicePort{Name: p.Name, Port: p.Port, Protocol: p.Protocol})
		}
		services = append(services, s)
	}
	return services, nil
}

// Workload is a Deployment or StatefulSet and the settings that decide whether replacing its pods
// interrupts traffic.
type Workload struct {
	Kind, Name     string
	Replicas       int
	Strategy       string // RollingUpdate or Recreate (Deployments), RollingUpdate or OnDelete (StatefulSets)
	MaxUnavailable string
	HasReadiness   bool
	HasPreStop     bool
	PodLabels      map[string]string
}

// Serves reports whether the Service selects this workload's pods.
func (w Workload) Serves(s Service) bool {
	if len(s.Selector) == 0 {
		return false
	}
	for k, v := range s.Selector {
		if w.PodLabels[k] != v {
			return false
		}
	}
	return true
}

// Workloads returns the Deployments and StatefulSets in ns.
func (c *Client) Workloads(ctx context.Context, ns string) ([]Workload, error) {
	out, err := c.Runner.Run(ctx, "kubectl", "--kubeconfig", c.Kubeconfig, "--namespace", ns, "get", "deployments,statefulsets", "-o", "json")
	if err != nil {
		return nil, err
	}
	var list struct {
		Items []struct {
			Kind     string `json:"kind"`
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Spec struct {
				Replicas *int `json:"replicas"`
				Strategy struct {
					Type          string `json:"type"`
					RollingUpdate struct {
						MaxUnavailable any `json:"maxUnavailable"`
					} `json:"rollingUpdate"`
				} `json:"strategy"`
				UpdateStrategy struct {
					Type string `json:"type"`
				} `json:"updateStrategy"`
				Template struct {
					Metadata struct {
						Labels map[string]string `json:"labels"`
					} `json:"metadata"`
					Spec struct {
						Containers []struct {
							ReadinessProbe *json.RawMessage `json:"readinessProbe"`
							Lifecycle      struct {
								PreStop *json.RawMessage `json:"preStop"`
							} `json:"lifecycle"`
						} `json:"containers"`
					} `json:"spec"`
				} `json:"template"`
			} `json:"spec"`
		} `json:"items"`
	}
	if err := json.Unmarshal(out, &list); err != nil {
		return nil, fmt.Errorf("parse workloads: %w", err)
	}
	var workloads []Workload
	for _, item := range list.Items {
		w := Workload{Kind: item.Kind, Name: item.Metadata.Name, Replicas: 1, Strategy: item.Spec.Strategy.Type,
			PodLabels: item.Spec.Template.Metadata.Labels}
		if item.Spec.Replicas != nil {
			w.Replicas = *item.Spec.Replicas
		}
		if item.Kind == "StatefulSet" {
			w.Strategy = item.Spec.UpdateStrategy.Type
		}
		if mu := item.Spec.Strategy.RollingUpdate.MaxUnavailable; mu != nil {
			w.MaxUnavailable = fmt.Sprint(mu)
		}
		for _, c := range item.Spec.Template.Spec.Containers {
			if c.ReadinessProbe != nil {
				w.HasReadiness = true
			}
			if c.Lifecycle.PreStop != nil {
				w.HasPreStop = true
			}
		}
		workloads = append(workloads, w)
	}
	return workloads, nil
}

// RolloutRestart replaces the pods of every Deployment and StatefulSet in ns and waits until the
// rollouts finish.
func (c *Client) RolloutRestart(ctx context.Context, ns string, workloads []Workload, timeout string) error {
	for _, w := range workloads {
		ref := strings.ToLower(w.Kind) + "/" + w.Name
		if _, err := c.Runner.Run(ctx, "kubectl", "--kubeconfig", c.Kubeconfig, "--namespace", ns, "rollout", "restart", ref); err != nil {
			return err
		}
	}
	for _, w := range workloads {
		ref := strings.ToLower(w.Kind) + "/" + w.Name
		if _, err := c.Runner.Run(ctx, "kubectl", "--kubeconfig", c.Kubeconfig, "--namespace", ns, "rollout", "status", ref, "--timeout", timeout); err != nil {
			return err
		}
	}
	return nil
}

// StartPod runs a pod with a shell script and returns once it is created.
func (c *Client) StartPod(ctx context.Context, ns, name, image, script string, labels map[string]string) error {
	args := []string{"--kubeconfig", c.Kubeconfig, "--namespace", ns, "run", name, "--image", image, "--restart", "Never"}
	var l []string
	for k, v := range labels {
		l = append(l, k+"="+v)
	}
	sort.Strings(l)
	if len(l) > 0 {
		args = append(args, "--labels", strings.Join(l, ","))
	}
	overrides, err := json.Marshal(hardenedPod(name))
	if err != nil {
		return err
	}
	args = append(args, "--override-type", "strategic", "--overrides", string(overrides))
	args = append(args, "--command", "--", "sh", "-c", script)
	_, err = c.Runner.Run(ctx, "kubectl", args...)
	return err
}

// hardenedPod is merged into the pod kubectl run makes. The pod lives in the chart's namespace,
// so it carries no ServiceAccount token and meets Pod Security "restricted": no root, no
// privilege escalation, no capabilities, a read-only root filesystem.
func hardenedPod(container string) map[string]any {
	return map[string]any{
		"spec": map[string]any{
			"automountServiceAccountToken": false,
			"securityContext": map[string]any{
				"runAsNonRoot":   true,
				"runAsUser":      65534,
				"runAsGroup":     65534,
				"seccompProfile": map[string]any{"type": "RuntimeDefault"},
			},
			"containers": []any{map[string]any{
				"name": container,
				"securityContext": map[string]any{
					"allowPrivilegeEscalation": false,
					"readOnlyRootFilesystem":   true,
					"capabilities":             map[string]any{"drop": []string{"ALL"}},
				},
			}},
		},
	}
}

// WaitPodRunning waits until the pod is running.
func (c *Client) WaitPodRunning(ctx context.Context, ns, name, timeout string) error {
	_, err := c.Runner.Run(ctx, "kubectl", "--kubeconfig", c.Kubeconfig, "--namespace", ns, "wait", "--for=jsonpath={.status.phase}=Running", "pod/"+name, "--timeout", timeout)
	return err
}

// Logs returns the logs of a pod.
func (c *Client) Logs(ctx context.Context, ns, name string) (string, error) {
	out, err := c.Runner.Run(ctx, "kubectl", "--kubeconfig", c.Kubeconfig, "--namespace", ns, "logs", name)
	return string(out), err
}

// LastLogLine returns the last line a pod has written so far.
func (c *Client) LastLogLine(ctx context.Context, ns, name string) (string, error) {
	out, err := c.Runner.Run(ctx, "kubectl", "--kubeconfig", c.Kubeconfig, "--namespace", ns, "logs", "--tail=1", name)
	return strings.TrimSpace(string(out)), err
}

// DeletePod removes a pod without waiting for it to go.
func (c *Client) DeletePod(ctx context.Context, ns, name string) error {
	_, err := c.Runner.Run(ctx, "kubectl", "--kubeconfig", c.Kubeconfig, "--namespace", ns, "delete", "pod", name, "--ignore-not-found", "--wait=false")
	return err
}
