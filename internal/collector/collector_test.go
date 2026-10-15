// SPDX-FileCopyrightText: 2026 Reza Jelveh
// SPDX-License-Identifier: Apache-2.0

package collector

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	listerscorev1 "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"

	"github.com/fishing-agents/hami-mock-monitor/internal/hami"
	"github.com/fishing-agents/hami-mock-monitor/internal/load"
)

const node = "gpu-node-1"

func resolver(t *testing.T, source load.Source) *load.Resolver {
	t.Helper()
	r, err := load.NewResolver(load.Config{
		Source: source, SeedPeriod: time.Minute, SeedMax: 100,
		StaticUtilization: 50, StaticMemoryFraction: 0.5,
		VLLMPort: 8000, VLLMPath: "/metrics", VLLMMaxNumSeqs: 5, ScrapeTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

type podOpt func(*corev1.Pod)

func newPod(name, devices string, opts ...podOpt) *corev1.Pod {
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "team-a", Name: name, UID: types.UID(name),
			Labels: map[string]string{hami.AssignedNodeLabel: node},
			Annotations: map[string]string{
				hami.BindPhaseAnnotation:        hami.BindPhaseSuccess,
				hami.DevicesAllocatedAnnotation: devices,
			},
		},
		Spec:   corev1.PodSpec{NodeName: node, Containers: []corev1.Container{{Name: "main"}}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
	for _, o := range opts {
		o(p)
	}
	return p
}

func annotate(k, v string) podOpt { return func(p *corev1.Pod) { p.Annotations[k] = v } }

func newCollector(t *testing.T, opts Options, loads *load.Resolver, pods ...*corev1.Pod) (*Collector, *prometheus.Registry) {
	t.Helper()
	indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	for _, p := range pods {
		if err := indexer.Add(p); err != nil {
			t.Fatal(err)
		}
	}
	opts.Node = node
	c := New(opts, listerscorev1.NewPodLister(indexer), loads)
	c.SetNodeDevices([]hami.NodeDevice{
		{ID: "GPU-0", Index: 0, MemoryMiB: 1024, Type: "NVIDIA-A100"},
		{ID: "GPU-1", Index: 1, MemoryMiB: 1024, Type: "NVIDIA-A100"},
		{ID: "GPU-2", Index: 2, MemoryMiB: 1024, Type: "NVIDIA-A100"},
	})
	reg := prometheus.NewPedanticRegistry()
	if err := Register(reg, c); err != nil {
		t.Fatal(err)
	}
	return c, reg
}

// gather flattens the registry into "name{k=v,...}" -> value, labels sorted.
func gather(t *testing.T, reg *prometheus.Registry) map[string]float64 {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]float64{}
	for _, mf := range families {
		for _, m := range mf.GetMetric() {
			var labels []string
			for _, l := range m.GetLabel() {
				labels = append(labels, l.GetName()+"="+l.GetValue())
			}
			slices.Sort(labels)
			out[mf.GetName()+"{"+strings.Join(labels, ",")+"}"] = m.GetGauge().GetValue()
		}
	}
	return out
}

func ctrKey(name, pod, ctr string, vdev int, uuid string) string {
	return fmt.Sprintf("%s{container=%s,device_uuid=%s,namespace=team-a,pod=%s,vdevice_index=%d,zone=vGPU}", name, ctr, uuid, pod, vdev)
}

func hostKey(name string, index int, uuid string) string {
	return fmt.Sprintf("%s{device_index=%d,device_type=NVIDIA-A100,device_uuid=%s,node=%s,zone=vGPU}", name, index, uuid, node)
}

func TestContractNamesAndLabels(t *testing.T) {
	p := newPod("llm", "GPU-0,NVIDIA,512,30:;", annotate(load.AnnotationSource, "static"))
	_, reg := newCollector(t, Options{}, resolver(t, load.SourceStatic), p)
	got := gather(t, reg)

	const mib = 1 << 20
	want := map[string]float64{
		ctrKey("hami_vgpu_memory_used_bytes", "llm", "main", 0, "GPU-0"):             256 * mib,
		ctrKey("hami_vgpu_memory_limit_bytes", "llm", "main", 0, "GPU-0"):            512 * mib,
		ctrKey("hami_container_device_memory_bytes", "llm", "main", 0, "GPU-0"):      256 * mib,
		ctrKey("hami_container_device_utilization_ratio", "llm", "main", 0, "GPU-0"): 15, // half of 30 cores
	}
	for i, uuid := range []string{"GPU-0", "GPU-1", "GPU-2"} {
		want[hostKey("hami_host_gpu_memory_used_bytes", i, uuid)] = 0
		want[hostKey("hami_host_gpu_utilization_ratio", i, uuid)] = 0
	}
	want[hostKey("hami_host_gpu_memory_used_bytes", 0, "GPU-0")] = 256 * mib
	want[hostKey("hami_host_gpu_utilization_ratio", 0, "GPU-0")] = 15
	assertSeries(t, got, want)
}

func assertSeries(t *testing.T, got, want map[string]float64) {
	t.Helper()
	for k, v := range want {
		if g, ok := got[k]; !ok || g != v {
			t.Errorf("%s = %v (present %v), want %v", k, g, ok, v)
		}
	}
	for k := range got {
		if _, ok := want[k]; !ok {
			t.Errorf("unexpected series %s", k)
		}
	}
}

func TestMultiContainerMultiDevice(t *testing.T) {
	p := newPod("multi", ";GPU-0,NVIDIA,512,0:GPU-1,NVIDIA,256,0:;GPU-2,NVIDIA,128,0:;", func(p *corev1.Pod) {
		p.Spec.InitContainers = []corev1.Container{{Name: "init"}}
		p.Spec.Containers = []corev1.Container{{Name: "main"}, {Name: "side"}}
	})
	_, reg := newCollector(t, Options{}, resolver(t, load.SourceStatic), p)
	got := gather(t, reg)
	const mib = 1 << 20
	for _, s := range []struct {
		ctr  string
		vdev int
		uuid string
		mem  float64
	}{{"main", 0, "GPU-0", 512}, {"main", 1, "GPU-1", 256}, {"side", 0, "GPU-2", 128}} {
		if got[ctrKey("hami_vgpu_memory_limit_bytes", "multi", s.ctr, s.vdev, s.uuid)] != s.mem*mib {
			t.Errorf("%s vdevice %d (%s): limit missing or wrong", s.ctr, s.vdev, s.uuid)
		}
	}
	for k := range got {
		if strings.Contains(k, "container=init") {
			t.Errorf("init container without devices emitted: %s", k)
		}
	}
}

func TestUtilizationIsShareOfDeviceAndHostCaps(t *testing.T) {
	// Two whole-device (cores 0) containers at 75% share GPU-0 and claim
	// 2 x 768 MiB of its 1024: both host sums hit their caps.
	a := newPod("a", "GPU-0,NVIDIA,768,0:;", annotate(load.AnnotationUtilization, "75"), annotate(load.AnnotationMemoryFraction, "1"))
	b := newPod("b", "GPU-0,NVIDIA,768,0:;", annotate(load.AnnotationUtilization, "75"), annotate(load.AnnotationMemoryFraction, "1"))
	// Half busy on 50 and on 20 cores: 25% and 10% of GPU-1.
	c := newPod("c", "GPU-1,NVIDIA,512,50:;")
	d := newPod("d", "GPU-1,NVIDIA,256,20:;")
	_, reg := newCollector(t, Options{}, resolver(t, load.SourceStatic), a, b, c, d)
	got := gather(t, reg)
	const mib = 1 << 20
	for k, want := range map[string]float64{
		ctrKey("hami_container_device_utilization_ratio", "a", "main", 0, "GPU-0"): 75,
		ctrKey("hami_container_device_utilization_ratio", "c", "main", 0, "GPU-1"): 25,
		ctrKey("hami_container_device_utilization_ratio", "d", "main", 0, "GPU-1"): 10,
		hostKey("hami_host_gpu_utilization_ratio", 0, "GPU-0"):                     100,
		hostKey("hami_host_gpu_memory_used_bytes", 0, "GPU-0"):                     1024 * mib,
		hostKey("hami_host_gpu_utilization_ratio", 1, "GPU-1"):                     35,
		hostKey("hami_host_gpu_memory_used_bytes", 1, "GPU-1"):                     384 * mib,
		hostKey("hami_host_gpu_utilization_ratio", 2, "GPU-2"):                     0,
	} {
		if got[k] != want {
			t.Errorf("%s = %v, want %v", k, got[k], want)
		}
	}
}

func TestOnlyBoundPodsCount(t *testing.T) {
	pods := []*corev1.Pod{
		newPod("bound", "GPU-0,NVIDIA,512,0:;"),
		newPod("allocating", "GPU-0,NVIDIA,512,0:;", annotate(hami.BindPhaseAnnotation, hami.BindPhaseAllocating)),
		newPod("failed-bind", "GPU-0,NVIDIA,512,0:;", annotate(hami.BindPhaseAnnotation, "failed")),
		newPod("done", "GPU-0,NVIDIA,512,0:;", func(p *corev1.Pod) { p.Status.Phase = corev1.PodSucceeded }),
		newPod("elsewhere", "GPU-0,NVIDIA,512,0:;", func(p *corev1.Pod) { p.Spec.NodeName = "other" }),
	}
	names := func(c *Collector) []string {
		var out []string
		for _, p := range c.Pods() {
			out = append(out, p.Name)
		}
		slices.Sort(out)
		return out
	}
	c, _ := newCollector(t, Options{}, resolver(t, load.SourceStatic), pods...)
	if got := names(c); !slices.Equal(got, []string{"bound"}) {
		t.Errorf("default: counted %v", got)
	}
	c, _ = newCollector(t, Options{CountAllocating: true}, resolver(t, load.SourceStatic), pods...)
	if got := names(c); !slices.Equal(got, []string{"allocating", "bound"}) {
		t.Errorf("--count-allocating: counted %v", got)
	}
}

func TestUnknownLoadDropsSeries(t *testing.T) {
	// vllm pods without a scraped sample are unknown: no container series, and
	// no host series for the device they share, rather than a made-up zero.
	p := newPod("llm", "GPU-0,NVIDIA,512,0:;", annotate(load.AnnotationSource, "vllm"))
	q := newPod("seeded", "GPU-1,NVIDIA,512,0:;", annotate(load.AnnotationSource, "seed"))
	_, reg := newCollector(t, Options{}, resolver(t, load.SourceStatic), p, q)
	got := gather(t, reg)
	for k := range got {
		if strings.Contains(k, "GPU-0") {
			t.Errorf("series for an unknown load: %s", k)
		}
	}
	if _, ok := got[ctrKey("hami_container_device_utilization_ratio", "seeded", "main", 0, "GPU-1")]; !ok {
		t.Error("a known pod lost its series")
	}
}

func TestLegacyMetrics(t *testing.T) {
	legacy := []string{
		"HostGPUMemoryUsage", "HostCoreUtilization",
		"vGPU_device_memory_usage_in_bytes", "vGPU_device_memory_limit_in_bytes",
		"Device_memory_desc_of_container", "Device_utilization_desc_of_container",
	}
	p := newPod("llm", "GPU-0,NVIDIA,512,0:;")
	for _, on := range []bool{false, true} {
		_, reg := newCollector(t, Options{Legacy: on}, resolver(t, load.SourceStatic), p)
		got := gather(t, reg)
		for _, name := range legacy {
			present := false
			for k := range got {
				present = present || strings.HasPrefix(k, name+"{")
			}
			if present != on {
				t.Errorf("legacy=%v: %s present=%v", on, name, present)
			}
		}
	}

	_, reg := newCollector(t, Options{Legacy: true}, resolver(t, load.SourceStatic), p)
	got := gather(t, reg)
	const mib = 1 << 20
	for k, want := range map[string]float64{
		"vGPU_device_memory_usage_in_bytes{ctrname=main,deviceuuid=GPU-0,podname=llm,podnamespace=team-a,vdeviceid=0,zone=vGPU}": 256 * mib,
		"HostCoreUtilization{deviceidx=0,devicetype=NVIDIA-A100,deviceuuid=GPU-0,nodeid=gpu-node-1,zone=vGPU}":                   50,
	} {
		if got[k] != want {
			t.Errorf("%s = %v, want %v", k, got[k], want)
		}
	}
}
