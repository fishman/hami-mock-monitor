// SPDX-FileCopyrightText: 2026 Reza Jelveh
// SPDX-License-Identifier: Apache-2.0

package load

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

var t0 = time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)

func testConfig() Config {
	return Config{
		Source:               SourceSeed,
		SeedPeriod:           10 * time.Minute,
		SeedMin:              10,
		SeedMax:              90,
		StaticUtilization:    40,
		StaticMemoryFraction: 0.5,
		VLLMPort:             8000,
		VLLMPath:             "/metrics",
		VLLMMaxNumSeqs:       5,
		ScrapeTimeout:        time.Second,
		StaleAfter:           time.Minute,
	}
}

func newResolver(t *testing.T, cfg Config) *Resolver {
	t.Helper()
	r, err := NewResolver(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func pod(name string, annotations map[string]string) *corev1.Pod {
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Namespace: "ns", Name: name, UID: types.UID("uid-" + name), Annotations: annotations,
	}}
}

func TestSeedIsDeterministic(t *testing.T) {
	a := newResolver(t, testConfig())
	b := newResolver(t, testConfig())
	p := pod("p", nil)
	for _, at := range []time.Time{t0, t0.Add(37 * time.Second), t0.Add(4 * time.Minute)} {
		la, err := a.Load(p, "c", "GPU-0", at)
		if err != nil {
			t.Fatal(err)
		}
		lb, _ := b.Load(p, "c", "GPU-0", at)
		if la != lb {
			t.Fatalf("same seed, different values at %v: %+v vs %+v", at, la, lb)
		}
		if la.ComputeFraction < 0.1 || la.ComputeFraction > 0.9 || la.MemoryFraction < 0.1 || la.MemoryFraction > 0.9 {
			t.Fatalf("value outside [min,max]: %+v", la)
		}
	}

	other := testConfig()
	other.Seed = 7
	c := newResolver(t, other)
	la, _ := a.Load(p, "c", "GPU-0", t0)
	lc, _ := c.Load(p, "c", "GPU-0", t0)
	if la == lc {
		t.Fatalf("different seeds gave the same value %+v", la)
	}
	ld, _ := a.Load(p, "c", "GPU-1", t0)
	if la == ld {
		t.Fatalf("different devices share a phase: %+v", la)
	}
	le, _ := a.Load(pod("p", map[string]string{AnnotationSeed: "7"}), "c", "GPU-0", t0)
	if le != lc {
		t.Fatalf("seed annotation not honoured: %+v vs %+v", le, lc)
	}
}

func TestStaticSourceAndAnnotations(t *testing.T) {
	r := newResolver(t, testConfig())
	got, err := r.Load(pod("p", map[string]string{AnnotationSource: "static"}), "c", "GPU-0", t0)
	if err != nil || got != (Load{ComputeFraction: 0.4, MemoryFraction: 0.5}) {
		t.Fatalf("defaults: %+v, %v", got, err)
	}
	got, err = r.Load(pod("p", map[string]string{
		AnnotationSource: "static", AnnotationUtilization: "75", AnnotationMemoryFraction: "0.25",
	}), "c", "GPU-0", t0)
	if err != nil || got != (Load{ComputeFraction: 0.75, MemoryFraction: 0.25}) {
		t.Fatalf("annotations: %+v, %v", got, err)
	}
	for _, a := range []map[string]string{
		{AnnotationSource: "static", AnnotationUtilization: "101"},
		{AnnotationSource: "static", AnnotationMemoryFraction: "1.5"},
		{AnnotationSource: "static", AnnotationUtilization: "busy"},
		{AnnotationSource: "dcgm"},
	} {
		if _, err := r.Load(pod("p", a), "c", "GPU-0", t0); err == nil || errors.Is(err, ErrNoSample) {
			t.Errorf("%v: want a configuration error, got %v", a, err)
		}
	}
}

// vllmServer serves vLLM metrics; fail switches it to HTTP 500.
func vllmServer(t *testing.T, body string) (port string, fail *atomic.Bool) {
	t.Helper()
	fail = new(atomic.Bool)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail.Load() || r.URL.Path != "/custom" {
			http.Error(w, "down", http.StatusInternalServerError)
			return
		}
		fmt.Fprint(w, body)
	}))
	t.Cleanup(srv.Close)
	_, port, _ = net.SplitHostPort(srv.Listener.Addr().String())
	return port, fail
}

const vllmMetrics = `# HELP vllm:num_requests_running Number of requests in model execution batches.
# TYPE vllm:num_requests_running gauge
vllm:num_requests_running{engine="0",model_name="m"} 3
# HELP vllm:kv_cache_usage_perc KV-cache usage. 1 means 100 percent usage.
# TYPE vllm:kv_cache_usage_perc gauge
vllm:kv_cache_usage_perc{engine="0",model_name="m"} 0.25
`

func vllmPod(name, port string, extra map[string]string) *corev1.Pod {
	a := map[string]string{AnnotationSource: "vllm", AnnotationMetricsPort: port, AnnotationMetricsPath: "/custom"}
	for k, v := range extra {
		a[k] = v
	}
	p := pod(name, a)
	p.Status.PodIP = "127.0.0.1"
	return p
}

func TestVLLMMapping(t *testing.T) {
	port, _ := vllmServer(t, vllmMetrics)
	r := newResolver(t, testConfig())
	defaultSeqs := vllmPod("a", port, nil)
	moreSeqs := vllmPod("b", port, map[string]string{AnnotationMaxNumSeqs: "12"})
	saturated := vllmPod("c", port, map[string]string{AnnotationMaxNumSeqs: "2"})
	r.Refresh(context.Background(), []*corev1.Pod{defaultSeqs, moreSeqs, saturated}, t0)

	for _, tc := range []struct {
		pod  *corev1.Pod
		want Load
	}{
		{defaultSeqs, Load{ComputeFraction: 0.6, MemoryFraction: 0.25}}, // 3 of 5
		{moreSeqs, Load{ComputeFraction: 0.25, MemoryFraction: 0.25}},   // 3 of 12
		{saturated, Load{ComputeFraction: 1, MemoryFraction: 0.25}},     // capped
	} {
		got, err := r.Load(tc.pod, "c", "GPU-0", t0)
		if err != nil || got != tc.want {
			t.Errorf("%s: got %+v, %v; want %+v", tc.pod.Name, got, err, tc.want)
		}
	}
}

func TestVLLMPortFromNamedContainerPort(t *testing.T) {
	port, _ := vllmServer(t, vllmMetrics)
	n, _ := strconv.Atoi(port)
	p := vllmPod("a", "", nil)
	delete(p.Annotations, AnnotationMetricsPort)
	p.Spec.Containers = []corev1.Container{{Name: "sim", Ports: []corev1.ContainerPort{
		{Name: "grpc", ContainerPort: 1}, {Name: "http", ContainerPort: int32(n)},
	}}}
	r := newResolver(t, testConfig())
	r.Refresh(context.Background(), []*corev1.Pod{p}, t0)
	if _, err := r.Load(p, "sim", "GPU-0", t0); err != nil {
		t.Fatalf("port named http not scraped: %v", err)
	}
}

func TestVLLMStaleness(t *testing.T) {
	port, fail := vllmServer(t, vllmMetrics)
	r := newResolver(t, testConfig()) // StaleAfter: 1m
	p := vllmPod("a", port, nil)
	ctx := context.Background()

	if _, err := r.Load(p, "c", "GPU-0", t0); !errors.Is(err, ErrNoSample) {
		t.Fatalf("before any scrape: want ErrNoSample, got %v", err)
	}
	r.Refresh(ctx, []*corev1.Pod{p}, t0)
	fail.Store(true)
	r.Refresh(ctx, []*corev1.Pod{p}, t0.Add(30*time.Second))
	got, err := r.Load(p, "c", "GPU-0", t0.Add(30*time.Second))
	if err != nil || got.ComputeFraction != 0.6 {
		t.Fatalf("within the staleness window: got %+v, %v; want the last value", got, err)
	}
	r.Refresh(ctx, []*corev1.Pod{p}, t0.Add(61*time.Second))
	if _, err := r.Load(p, "c", "GPU-0", t0.Add(61*time.Second)); !errors.Is(err, ErrNoSample) {
		t.Fatalf("past the staleness window: want ErrNoSample, got %v", err)
	}

	fail.Store(false)
	r.Refresh(ctx, []*corev1.Pod{p}, t0.Add(90*time.Second))
	r.Refresh(ctx, nil, t0.Add(91*time.Second)) // pod gone
	if _, err := r.Load(p, "c", "GPU-0", t0.Add(91*time.Second)); !errors.Is(err, ErrNoSample) {
		t.Fatalf("sample of a vanished pod kept: %v", err)
	}
}

func TestVLLMMissingMetricIsNoSample(t *testing.T) {
	port, _ := vllmServer(t, "# TYPE vllm:num_requests_running gauge\nvllm:num_requests_running 1\n")
	r := newResolver(t, testConfig())
	p := vllmPod("a", port, nil)
	r.Refresh(context.Background(), []*corev1.Pod{p}, t0)
	if _, err := r.Load(p, "c", "GPU-0", t0); !errors.Is(err, ErrNoSample) {
		t.Fatalf("without kv cache usage: want ErrNoSample, got %v", err)
	}
}
