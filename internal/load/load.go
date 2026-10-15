// SPDX-FileCopyrightText: 2026 Reza Jelveh
// SPDX-License-Identifier: Apache-2.0

// Package load produces the usage behind the monitor's metrics. Each pod picks
// a source by annotation, falling back to the configured default:
//   - vllm: scraped from the pod's vLLM-compatible /metrics endpoint
//   - seed: a deterministic sine curve, phase-shifted per pod, container and device
//   - static: fixed values
package load

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
)

type Source string

const (
	SourceVLLM   Source = "vllm"
	SourceSeed   Source = "seed"
	SourceStatic Source = "static"
)

const annotationPrefix = "hami-mock-monitor.io/"

// Per-pod annotations; each overrides the matching flag.
const (
	AnnotationSource         = annotationPrefix + "load-source"
	AnnotationMetricsPort    = annotationPrefix + "metrics-port"
	AnnotationMetricsPath    = annotationPrefix + "metrics-path"
	AnnotationMaxNumSeqs     = annotationPrefix + "max-num-seqs"
	AnnotationUtilization    = annotationPrefix + "utilization"
	AnnotationMemoryFraction = annotationPrefix + "memory-fraction"
	AnnotationSeed           = annotationPrefix + "seed"
)

// ErrNoSample means a vllm pod has no sample younger than the staleness window.
var ErrNoSample = errors.New("no fresh vllm sample")

// maxBody bounds a scraped response; vLLM's histograms stay far below it.
const maxBody = 16 << 20

// Load is the usage of one container device.
type Load struct {
	Utilization    float64 // SM utilization, 0-100
	MemoryFraction float64 // share of the vGPU memory limit in use, 0-1
}

type Config struct {
	Source Source

	Seed       int64
	SeedPeriod time.Duration
	// SeedMin and SeedMax bound the curve in percent, for utilization and
	// memory alike.
	SeedMin, SeedMax float64

	StaticUtilization    float64
	StaticMemoryFraction float64

	VLLMPort       int
	VLLMPath       string
	VLLMMaxNumSeqs float64
	ScrapeTimeout  time.Duration
	// StaleAfter is how long a vllm pod's last sample outlives failing scrapes.
	StaleAfter time.Duration
}

type sample struct {
	load Load
	at   time.Time
}

// Resolver answers loads for every source and caches vllm samples by pod UID.
type Resolver struct {
	cfg    Config
	client *http.Client

	mu      sync.RWMutex
	samples map[types.UID]sample
}

func NewResolver(cfg Config) (*Resolver, error) {
	if _, err := parseSource(string(cfg.Source)); err != nil {
		return nil, err
	}
	switch {
	case cfg.SeedPeriod <= 0:
		return nil, errors.New("seed period must be positive")
	case !(0 <= cfg.SeedMin && cfg.SeedMin <= cfg.SeedMax && cfg.SeedMax <= 100):
		return nil, errors.New("seed bounds must satisfy 0 <= min <= max <= 100")
	case !(cfg.StaticUtilization >= 0 && cfg.StaticUtilization <= 100):
		return nil, errors.New("static utilization must be in [0, 100]")
	case !(cfg.StaticMemoryFraction >= 0 && cfg.StaticMemoryFraction <= 1):
		return nil, errors.New("static memory fraction must be in [0, 1]")
	case cfg.VLLMPort < 1 || cfg.VLLMPort > 65535:
		return nil, errors.New("vllm port must be in [1, 65535]")
	case !strings.HasPrefix(cfg.VLLMPath, "/"):
		return nil, errors.New("vllm metrics path must start with /")
	case !(cfg.VLLMMaxNumSeqs >= 1):
		return nil, errors.New("vllm max-num-seqs must be at least 1")
	case cfg.ScrapeTimeout <= 0:
		return nil, errors.New("scrape timeout must be positive")
	case cfg.StaleAfter < 0:
		return nil, errors.New("staleness window must not be negative")
	}
	return &Resolver{
		cfg:     cfg,
		client:  &http.Client{Timeout: cfg.ScrapeTimeout},
		samples: make(map[types.UID]sample),
	}, nil
}

func parseSource(s string) (Source, error) {
	switch src := Source(s); src {
	case SourceVLLM, SourceSeed, SourceStatic:
		return src, nil
	}
	return "", fmt.Errorf("unknown load source %q (want vllm, seed or static)", s)
}

func (r *Resolver) source(pod *corev1.Pod) (Source, error) {
	return parseSource(cmp.Or(pod.Annotations[AnnotationSource], string(r.cfg.Source)))
}

// Load returns the usage of one device of a container. ErrNoSample marks an
// unknown vllm value; any other error is a pod misconfiguration.
func (r *Resolver) Load(pod *corev1.Pod, container, uuid string, now time.Time) (Load, error) {
	src, err := r.source(pod)
	if err != nil {
		return Load{}, err
	}
	switch src {
	case SourceStatic:
		u, err := annotationFloat(pod, AnnotationUtilization, r.cfg.StaticUtilization, 0, 100)
		if err != nil {
			return Load{}, err
		}
		f, err := annotationFloat(pod, AnnotationMemoryFraction, r.cfg.StaticMemoryFraction, 0, 1)
		if err != nil {
			return Load{}, err
		}
		return Load{Utilization: u, MemoryFraction: f}, nil
	case SourceSeed:
		seed := r.cfg.Seed
		if v, ok := pod.Annotations[AnnotationSeed]; ok {
			if seed, err = strconv.ParseInt(v, 10, 64); err != nil {
				return Load{}, fmt.Errorf("annotation %s=%q: want an integer", AnnotationSeed, v)
			}
		}
		key := pod.Namespace + "/" + pod.Name + "/" + container + "/" + uuid
		lo, span := r.cfg.SeedMin, r.cfg.SeedMax-r.cfg.SeedMin
		return Load{
			Utilization:    lo + span*wave(seed, key+"/utilization", r.cfg.SeedPeriod, now),
			MemoryFraction: (lo + span*wave(seed, key+"/memory", r.cfg.SeedPeriod, now)) / 100,
		}, nil
	default:
		r.mu.RLock()
		s, ok := r.samples[pod.UID]
		r.mu.RUnlock()
		if !ok || now.Sub(s.at) > r.cfg.StaleAfter {
			return Load{}, ErrNoSample
		}
		return s.load, nil
	}
}

// wave is a sine over period scaled to [0, 1], phase-shifted by
// sha256(seed, key) so every series follows its own reproducible curve.
func wave(seed int64, key string, period time.Duration, now time.Time) float64 {
	sum := sha256.Sum256(fmt.Appendf(nil, "%d:%s", seed, key))
	p := uint64(period)
	pos := (uint64(now.UnixNano()) + binary.BigEndian.Uint64(sum[:8])%p) % p
	return (math.Sin(2*math.Pi*float64(pos)/float64(p)) + 1) / 2
}

// Refresh scrapes every vllm pod among pods concurrently. A failed scrape
// keeps the previous sample; pods no longer listed lose theirs.
func (r *Resolver) Refresh(ctx context.Context, pods []*corev1.Pod, now time.Time) {
	live := make(map[types.UID]bool)
	fresh := make([]*Load, len(pods))
	var wg sync.WaitGroup
	for i, pod := range pods {
		if src, err := r.source(pod); err != nil || src != SourceVLLM {
			continue
		}
		live[pod.UID] = true
		wg.Go(func() {
			l, err := r.scrape(ctx, pod)
			if err != nil {
				slog.Warn("vllm scrape failed", "pod", pod.Namespace+"/"+pod.Name, "err", err)
				return
			}
			fresh[i] = &l
		})
	}
	wg.Wait()

	r.mu.Lock()
	defer r.mu.Unlock()
	for uid := range r.samples {
		if !live[uid] {
			delete(r.samples, uid)
		}
	}
	for i, l := range fresh {
		if l != nil {
			r.samples[pods[i].UID] = sample{load: *l, at: now}
		}
	}
}

func (r *Resolver) scrape(ctx context.Context, pod *corev1.Pod) (Load, error) {
	maxSeqs, err := annotationFloat(pod, AnnotationMaxNumSeqs, r.cfg.VLLMMaxNumSeqs, 1, math.Inf(1))
	if err != nil {
		return Load{}, err
	}
	target, err := r.metricsURL(pod)
	if err != nil {
		return Load{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return Load{}, err
	}
	req.Header.Set("Accept", "text/plain;version=0.0.4")
	resp, err := r.client.Do(req)
	if err != nil {
		return Load{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Load{}, fmt.Errorf("GET %s: %s", target, resp.Status)
	}
	parser := expfmt.NewTextParser(model.UTF8Validation)
	families, err := parser.TextToMetricFamilies(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return Load{}, fmt.Errorf("parse %s: %w", target, err)
	}
	running, ok := aggregate(families["vllm:num_requests_running"], false)
	if !ok {
		return Load{}, fmt.Errorf("%s: no vllm:num_requests_running", target)
	}
	kv, ok := aggregate(families["vllm:kv_cache_usage_perc"], true)
	if !ok {
		// vLLM before 0.10 named it gpu_cache_usage_perc.
		if kv, ok = aggregate(families["vllm:gpu_cache_usage_perc"], true); !ok {
			return Load{}, fmt.Errorf("%s: no vllm:kv_cache_usage_perc", target)
		}
	}
	return Load{
		Utilization:    min(100, max(0, running/maxSeqs*100)),
		MemoryFraction: min(1, max(0, kv)),
	}, nil
}

// aggregate sums (or averages) a family's series, e.g. one per engine.
func aggregate(mf *dto.MetricFamily, mean bool) (float64, bool) {
	if mf == nil || len(mf.GetMetric()) == 0 {
		return 0, false
	}
	var sum float64
	for _, m := range mf.GetMetric() {
		switch {
		case m.Gauge != nil:
			sum += m.GetGauge().GetValue()
		case m.Untyped != nil:
			sum += m.GetUntyped().GetValue()
		default:
			return 0, false
		}
	}
	if mean {
		sum /= float64(len(mf.GetMetric()))
	}
	return sum, true
}

// metricsURL resolves the port from the annotation, else the first container
// port named metrics, else one named http, else the configured default.
func (r *Resolver) metricsURL(pod *corev1.Pod) (string, error) {
	if pod.Status.PodIP == "" {
		return "", errors.New("pod has no IP yet")
	}
	port := r.cfg.VLLMPort
	if v, ok := pod.Annotations[AnnotationMetricsPort]; ok {
		p, err := strconv.Atoi(v)
		if err != nil || p < 1 || p > 65535 {
			return "", fmt.Errorf("annotation %s=%q: want a port", AnnotationMetricsPort, v)
		}
		port = p
	} else if p := namedPort(pod); p != 0 {
		port = p
	}
	path := cmp.Or(pod.Annotations[AnnotationMetricsPath], r.cfg.VLLMPath)
	if !strings.HasPrefix(path, "/") {
		return "", fmt.Errorf("annotation %s=%q: want an absolute path", AnnotationMetricsPath, path)
	}
	u := url.URL{Scheme: "http", Host: net.JoinHostPort(pod.Status.PodIP, strconv.Itoa(port)), Path: path}
	return u.String(), nil
}

func namedPort(pod *corev1.Pod) int {
	var http int32
	for _, c := range pod.Spec.Containers {
		for _, p := range c.Ports {
			switch {
			case p.Name == "metrics":
				return int(p.ContainerPort)
			case p.Name == "http" && http == 0:
				http = p.ContainerPort
			}
		}
	}
	return int(http)
}

func annotationFloat(pod *corev1.Pod, key string, def, lo, hi float64) (float64, error) {
	v, ok := pod.Annotations[key]
	if !ok {
		return def, nil
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || !(f >= lo && f <= hi) {
		return 0, fmt.Errorf("annotation %s=%q: want a number in [%g, %g]", key, v, lo, hi)
	}
	return f, nil
}
