// SPDX-FileCopyrightText: 2026 Reza Jelveh
// SPDX-License-Identifier: Apache-2.0

package webhook

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	jsonpatch "gopkg.in/evanphx/json-patch.v4"
	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/fishing-agents/hami-mock-monitor/internal/hami"
)

const smiURL = "http://hami-mock-monitor.kube-system.svc:9394"

var injector = Injector{Image: "hami-mock-monitor:dev", URL: smiURL, Resources: DefaultResources}

// admit sends pod through ServeHTTP and returns it as the API server would
// store it after applying the patch.
func admit(t *testing.T, pod *corev1.Pod) (*corev1.Pod, bool) {
	t.Helper()
	raw, err := json.Marshal(pod)
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(admissionv1.AdmissionReview{
		TypeMeta: metav1.TypeMeta{APIVersion: "admission.k8s.io/v1", Kind: "AdmissionReview"},
		Request: &admissionv1.AdmissionRequest{
			UID: "req-1", Kind: metav1.GroupVersionKind{Version: "v1", Kind: "Pod"},
			Operation: admissionv1.Create, Namespace: "team-a", Object: runtime.RawExtension{Raw: raw},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	injector.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/inject", bytes.NewReader(body)))
	var review admissionv1.AdmissionReview
	if err := json.Unmarshal(rec.Body.Bytes(), &review); err != nil {
		t.Fatalf("%d %s: %v", rec.Code, rec.Body, err)
	}
	resp := review.Response
	if review.Kind != "AdmissionReview" || resp == nil || resp.UID != "req-1" || !resp.Allowed {
		t.Fatalf("response %+v", review)
	}
	if resp.Patch == nil {
		return pod, false
	}
	patch, err := jsonpatch.DecodePatch(resp.Patch)
	if err != nil {
		t.Fatal(err)
	}
	patched, err := patch.Apply(raw)
	if err != nil {
		t.Fatal(err)
	}
	var out corev1.Pod
	if err := json.Unmarshal(patched, &out); err != nil {
		t.Fatal(err)
	}
	return &out, true
}

func assertInjected(t *testing.T, c corev1.Container) {
	t.Helper()
	var mounted bool
	for _, m := range c.VolumeMounts {
		mounted = mounted || m.Name == name && m.MountPath == ShimPath && m.SubPath == "nvidia-smi" && m.ReadOnly
	}
	if !mounted {
		t.Errorf("%s: shim not mounted: %+v", c.Name, c.VolumeMounts)
	}
	env := map[string]corev1.EnvVar{}
	for _, e := range c.Env {
		env[e.Name] = e
	}
	if env["HAMI_MOCK_SMI_URL"].Value != smiURL || env["HAMI_MOCK_SMI_CONTAINER"].Value != c.Name {
		t.Errorf("%s: env %+v", c.Name, c.Env)
	}
	for k, path := range map[string]string{"POD_NAMESPACE": "metadata.namespace", "POD_NAME": "metadata.name"} {
		if v := env[k].ValueFrom; v == nil || v.FieldRef == nil || v.FieldRef.FieldPath != path {
			t.Errorf("%s: %s not from %s: %+v", c.Name, k, path, env[k])
		}
	}
}

func TestAllocatedPodGetsShim(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "train", Namespace: "team-a",
			Annotations: map[string]string{hami.DevicesAllocatedAnnotation: ";GPU-0,NVIDIA,1024,0:;;"}},
		Spec: corev1.PodSpec{
			InitContainers: []corev1.Container{{Name: "fetch", Image: "busybox"}},
			Containers: []corev1.Container{
				{Name: "trainer", Image: "pytorch", Env: []corev1.EnvVar{{Name: "EPOCHS", Value: "3"}}},
				{Name: "eval", Image: "pytorch", VolumeMounts: []corev1.VolumeMount{{Name: "data", MountPath: "/data"}}},
			},
			Volumes: []corev1.Volume{{Name: "data"}},
		},
	}
	got, patched := admit(t, pod)
	if !patched {
		t.Fatal("allocated pod not patched")
	}
	if len(got.Spec.InitContainers) != 2 || len(got.Spec.Containers) != 2 {
		t.Fatalf("containers: %+v", got.Spec)
	}
	init := got.Spec.InitContainers[0]
	if init.Name != name || init.Image != "hami-mock-monitor:dev" || len(init.Args) != 2 || init.Args[0] != "install-smi" || init.Args[1] != shimDir+"/nvidia-smi" {
		t.Errorf("init container %+v", init)
	}
	if len(init.VolumeMounts) != 1 || init.VolumeMounts[0].Name != name || init.VolumeMounts[0].MountPath != shimDir {
		t.Errorf("init container mounts %+v", init.VolumeMounts)
	}
	if got.Spec.InitContainers[1].Name != "fetch" {
		t.Errorf("own init container moved: %+v", got.Spec.InitContainers)
	}
	for _, c := range append(got.Spec.InitContainers[1:], got.Spec.Containers...) {
		assertInjected(t, c)
	}
	if e := got.Spec.Containers[0].Env[0]; e.Name != "EPOCHS" {
		t.Errorf("own env lost: %+v", got.Spec.Containers[0].Env)
	}
	if m := got.Spec.Containers[1].VolumeMounts[0]; m.Name != "data" {
		t.Errorf("own mount lost: %+v", got.Spec.Containers[1].VolumeMounts)
	}
	if len(got.Spec.Volumes) != 2 || got.Spec.Volumes[0].Name != "data" || got.Spec.Volumes[1].Name != name || got.Spec.Volumes[1].EmptyDir == nil {
		t.Errorf("volumes %+v", got.Spec.Volumes)
	}
	if _, again := admit(t, got); again {
		t.Error("injected twice")
	}
}

// At creation HAMi has not allocated yet; its resource limits mark the pod.
func TestPodRequestingHAMiResourceGetsShim(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "sim", Namespace: "team-a"},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name: "sim", Env: []corev1.EnvVar{{Name: "POD_NAME", Value: "custom"}},
			Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{"nvidia.com/gpumem": resource.MustParse("13653")}},
		}}},
	}
	got, patched := admit(t, pod)
	if !patched {
		t.Fatal("pod limiting nvidia.com/gpumem not patched")
	}
	c := got.Spec.Containers[0]
	var names []string
	for _, e := range c.Env {
		names = append(names, e.Name)
	}
	if c.Env[0].Value != "custom" || len(c.Env) != 4 {
		t.Errorf("own POD_NAME must win, env %v", names)
	}
}

func TestUnallocatedPodUnchanged(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "team-a"},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name: "web", Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")}},
		}}},
	}
	if _, patched := admit(t, pod); patched {
		t.Error("pod without allocation patched")
	}
}
