// SPDX-FileCopyrightText: 2026 Reza Jelveh
// SPDX-License-Identifier: Apache-2.0

// Package webhook is a mutating admission webhook that puts the mock
// nvidia-smi into the containers of pods HAMi allocates devices to: an init
// container copies the binary into an emptyDir, each container mounts it as
// /usr/local/bin/nvidia-smi and gets the environment the shim needs.
package webhook

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"slices"

	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"

	"github.com/fishing-agents/hami-mock-monitor/internal/hami"
)

const (
	// name is the volume and init container; a pod with the volume already
	// has the shim.
	name    = "hami-mock-smi"
	shimDir = "/hami-mock-smi"
	// ShimPath is where containers find nvidia-smi.
	ShimPath = "/usr/local/bin/nvidia-smi"
	maxBody  = 4 << 20
)

// DefaultResources are HAMi's NVIDIA resource names. HAMi writes a pod's
// allocation only when scheduling it, after admission, so at creation a
// limit on one of them is what marks a pod HAMi allocates to.
var DefaultResources = []corev1.ResourceName{"nvidia.com/gpu", "nvidia.com/gpumem", "nvidia.com/gpucores", "nvidia.com/gpumem-percentage"}

type Injector struct {
	// Image runs the init container copying the shim: the monitor's own.
	Image string
	// URL is the monitor the shim asks, a Service routing to the node's own.
	URL       string
	Resources []corev1.ResourceName
}

// ServeHTTP answers AdmissionReview requests for pods. It never denies: a
// pod it cannot read is admitted unchanged.
func (in Injector) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var review admissionv1.AdmissionReview
	if err := json.NewDecoder(io.LimitReader(r.Body, maxBody)).Decode(&review); err != nil || review.Request == nil {
		http.Error(w, "expected an AdmissionReview request", http.StatusBadRequest)
		return
	}
	req := review.Request
	resp := &admissionv1.AdmissionResponse{UID: req.UID, Allowed: true}
	if req.Kind.Group == "" && req.Kind.Kind == "Pod" && req.Operation == admissionv1.Create {
		var pod corev1.Pod
		var patch []byte
		err := json.Unmarshal(req.Object.Raw, &pod)
		if err == nil {
			patch, err = in.Patch(&pod)
		}
		switch {
		case err != nil:
			slog.Warn("admit pod unchanged", "namespace", req.Namespace, "err", err)
		case patch != nil:
			pt := admissionv1.PatchTypeJSONPatch
			resp.Patch, resp.PatchType = patch, &pt
		}
	}
	review.Request, review.Response = nil, resp
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(review); err != nil {
		slog.Warn("write admission response", "err", err)
	}
}

type patchOp struct {
	Op    string `json:"op"`
	Path  string `json:"path"`
	Value any    `json:"value"`
}

// Patch returns the JSON patch injecting the shim, nil for a pod HAMi does
// not allocate to or that has the shim already.
func (in Injector) Patch(pod *corev1.Pod) ([]byte, error) {
	if !in.allocated(pod) || slices.ContainsFunc(pod.Spec.Volumes, func(v corev1.Volume) bool { return v.Name == name }) {
		return nil, nil
	}
	initContainers := []corev1.Container{{
		Name:         name,
		Image:        in.Image,
		Args:         []string{"install-smi", shimDir + "/nvidia-smi"},
		VolumeMounts: []corev1.VolumeMount{{Name: name, MountPath: shimDir}},
		SecurityContext: &corev1.SecurityContext{
			AllowPrivilegeEscalation: new(false),
			ReadOnlyRootFilesystem:   new(true),
			Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		},
	}}
	for _, c := range pod.Spec.InitContainers {
		initContainers = append(initContainers, in.mount(c))
	}
	var containers []corev1.Container
	for _, c := range pod.Spec.Containers {
		containers = append(containers, in.mount(c))
	}
	volumes := append(slices.Clip(pod.Spec.Volumes), corev1.Volume{Name: name, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}})
	return json.Marshal([]patchOp{
		{Op: "add", Path: "/spec/volumes", Value: volumes},
		{Op: "add", Path: "/spec/initContainers", Value: initContainers},
		{Op: "add", Path: "/spec/containers", Value: containers},
	})
}

func (in Injector) allocated(pod *corev1.Pod) bool {
	if pod.Annotations[hami.DevicesAllocatedAnnotation] != "" {
		return true
	}
	for _, c := range slices.Concat(pod.Spec.InitContainers, pod.Spec.Containers) {
		for _, r := range in.Resources {
			if _, ok := c.Resources.Limits[r]; ok {
				return true
			}
		}
	}
	return false
}

// mount adds the shim and its environment to c. What c defines itself wins:
// a second mount on the same path would make the pod invalid.
func (in Injector) mount(c corev1.Container) corev1.Container {
	c.VolumeMounts, c.Env = slices.Clone(c.VolumeMounts), slices.Clone(c.Env)
	if !slices.ContainsFunc(c.VolumeMounts, func(m corev1.VolumeMount) bool { return m.MountPath == ShimPath }) {
		c.VolumeMounts = append(c.VolumeMounts, corev1.VolumeMount{Name: name, MountPath: ShimPath, SubPath: "nvidia-smi", ReadOnly: true})
	}
	for _, e := range []corev1.EnvVar{
		{Name: "HAMI_MOCK_SMI_URL", Value: in.URL},
		{Name: "POD_NAMESPACE", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.namespace"}}},
		{Name: "POD_NAME", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.name"}}},
		{Name: "HAMI_MOCK_SMI_CONTAINER", Value: c.Name},
	} {
		if !slices.ContainsFunc(c.Env, func(x corev1.EnvVar) bool { return x.Name == e.Name }) {
			c.Env = append(c.Env, e)
		}
	}
	return c
}
