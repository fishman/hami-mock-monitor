// SPDX-FileCopyrightText: 2026 Reza Jelveh
// SPDX-License-Identifier: Apache-2.0

// Package collector serves HAMi's vGPUmonitor metric contract (v2.10) for the
// pods HAMi bound to this node, from their allocation annotations and a load
// source instead of HAMi-core's shared memory and NVML.
package collector

import (
	"errors"
	"log/slog"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	listerscorev1 "k8s.io/client-go/listers/core/v1"

	"github.com/fishing-agents/hami-mock-monitor/internal/hami"
	"github.com/fishing-agents/hami-mock-monitor/internal/load"
)

const mib = 1 << 20

var (
	hostLabels      = []string{"node", "device_index", "device_uuid", "device_type"}
	containerLabels = []string{"namespace", "pod", "container", "vdevice_index", "device_uuid"}

	hostMemoryUsed  = prometheus.NewDesc("hami_host_gpu_memory_used_bytes", "GPU device memory usage in bytes", hostLabels, nil)
	hostUtilization = prometheus.NewDesc("hami_host_gpu_utilization_ratio", "GPU core utilization ratio (0-100)", hostLabels, nil)
	vgpuMemoryUsed  = prometheus.NewDesc("hami_vgpu_memory_used_bytes", "vGPU device memory usage in bytes", containerLabels, nil)
	vgpuMemoryLimit = prometheus.NewDesc("hami_vgpu_memory_limit_bytes", "vGPU device memory limit in bytes", containerLabels, nil)
	ctrMemory       = prometheus.NewDesc("hami_container_device_memory_bytes", "Container device memory usage in bytes", containerLabels, nil)
	ctrUtilization  = prometheus.NewDesc("hami_container_device_utilization_ratio", "Container device SM utilization ratio", containerLabels, nil)

	legacyHostLabels      = []string{"nodeid", "deviceidx", "deviceuuid", "devicetype"}
	legacyContainerLabels = []string{"podnamespace", "podname", "ctrname", "vdeviceid", "deviceuuid"}

	legacyHostMemoryUsed  = prometheus.NewDesc("HostGPUMemoryUsage", "GPU device memory usage", legacyHostLabels, nil)
	legacyHostUtilization = prometheus.NewDesc("HostCoreUtilization", "GPU core utilization", legacyHostLabels, nil)
	legacyVGPUMemoryUsed  = prometheus.NewDesc("vGPU_device_memory_usage_in_bytes", "vGPU device usage", legacyContainerLabels, nil)
	legacyVGPUMemoryLimit = prometheus.NewDesc("vGPU_device_memory_limit_in_bytes", "vGPU device limit", legacyContainerLabels, nil)
	legacyCtrMemory       = prometheus.NewDesc("Device_memory_desc_of_container", "Container device memory description",
		[]string{"podnamespace", "podname", "ctrname", "vdeviceid", "deviceuuid", "context", "module", "data", "offset"}, nil)
	legacyCtrUtilization = prometheus.NewDesc("Device_utilization_desc_of_container", "Container device utilization description", legacyContainerLabels, nil)
)

type Options struct {
	Node string
	// CountAllocating also counts pods whose bind phase never left
	// "allocating", which is where HAMi's mock device plugin leaves them.
	CountAllocating bool
	// Legacy adds HAMi's legacy metric names (vGPUmonitor --legacy-metrics)
	// alongside the current ones.
	Legacy bool
}

type Collector struct {
	opts    Options
	pods    listerscorev1.PodLister
	loads   *load.Resolver
	devices atomic.Pointer[[]hami.NodeDevice]
	now     func() time.Time
}

func New(opts Options, pods listerscorev1.PodLister, loads *load.Resolver) *Collector {
	return &Collector{opts: opts, pods: pods, loads: loads, now: time.Now}
}

// Register adds c under the zone="vGPU" label HAMi's monitor carries.
func Register(reg prometheus.Registerer, c *Collector) error {
	return prometheus.WrapRegistererWith(prometheus.Labels{"zone": "vGPU"}, reg).Register(c)
}

// SetNodeDevices replaces the node's registered cards; nil drops host series.
func (c *Collector) SetNodeDevices(d []hami.NodeDevice) { c.devices.Store(&d) }

// Pods lists the pods HAMi bound to this node that still run.
func (c *Collector) Pods() []*corev1.Pod {
	pods, err := c.pods.List(labels.SelectorFromSet(labels.Set{hami.AssignedNodeLabel: c.opts.Node}))
	if err != nil {
		slog.Error("list pods", "err", err)
		return nil
	}
	counted := pods[:0:0]
	for _, p := range pods {
		if p.Spec.NodeName != c.opts.Node || p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
			continue
		}
		switch p.Annotations[hami.BindPhaseAnnotation] {
		case hami.BindPhaseSuccess:
		case hami.BindPhaseAllocating:
			if !c.opts.CountAllocating {
				continue
			}
		default:
			continue
		}
		counted = append(counted, p)
	}
	return counted
}

func (c *Collector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{hostMemoryUsed, hostUtilization, vgpuMemoryUsed, vgpuMemoryLimit, ctrMemory, ctrUtilization} {
		ch <- d
	}
	if c.opts.Legacy {
		for _, d := range []*prometheus.Desc{legacyHostMemoryUsed, legacyHostUtilization, legacyVGPUMemoryUsed, legacyVGPUMemoryLimit, legacyCtrMemory, legacyCtrUtilization} {
			ch <- d
		}
	}
}

type deviceUsage struct {
	utilization, memory float64
	// unknown marks a device with a container whose load is unknown: its sum
	// would understate, so the host series are left out.
	unknown bool
}

func (c *Collector) Collect(ch chan<- prometheus.Metric) {
	now := c.now()
	usage := map[string]*deviceUsage{}
	for _, pod := range c.Pods() {
		allocation, err := hami.DecodePodDevices(pod.Annotations[hami.DevicesAllocatedAnnotation])
		if err != nil {
			slog.Warn("skip pod with undecodable allocation", "pod", pod.Namespace+"/"+pod.Name, "err", err)
			continue
		}
		for i, devices := range allocation {
			if len(devices) == 0 {
				continue
			}
			ctr, ok := hami.ContainerName(pod, i)
			if !ok {
				slog.Warn("allocation names a missing container", "pod", pod.Namespace+"/"+pod.Name, "index", i)
				continue
			}
			for vdev, d := range devices {
				u := usage[d.UUID]
				if u == nil {
					u = &deviceUsage{}
					usage[d.UUID] = u
				}
				l, err := c.loads.Load(pod, ctr, d.UUID, now)
				if err != nil {
					u.unknown = true
					if !errors.Is(err, load.ErrNoSample) {
						slog.Warn("skip container with invalid load configuration", "pod", pod.Namespace+"/"+pod.Name, "container", ctr, "err", err)
					}
					continue
				}
				limit := float64(d.MemoryMiB) * mib
				used := l.MemoryFraction * limit
				// Like HAMi's monitor, utilization is in percent of the whole
				// device; cores 0 means the allocation may use all of it.
				cores := float64(d.Cores)
				if cores == 0 || cores > 100 {
					cores = 100
				}
				utilization := l.ComputeFraction * cores
				u.utilization += utilization
				u.memory += used

				lv := []string{pod.Namespace, pod.Name, ctr, strconv.Itoa(vdev), d.UUID}
				gauge(ch, vgpuMemoryUsed, used, lv...)
				gauge(ch, vgpuMemoryLimit, limit, lv...)
				gauge(ch, ctrMemory, used, lv...)
				gauge(ch, ctrUtilization, utilization, lv...)
				if c.opts.Legacy {
					gauge(ch, legacyVGPUMemoryUsed, used, lv...)
					gauge(ch, legacyVGPUMemoryLimit, limit, lv...)
					// context, module and data sizes are 0, so all of it is offset.
					gauge(ch, legacyCtrMemory, used, append(lv, "0", "0", "0", strconv.FormatFloat(used, 'f', 0, 64))...)
					gauge(ch, legacyCtrUtilization, utilization, lv...)
				}
			}
		}
	}

	devices := c.devices.Load()
	if devices == nil {
		return
	}
	for _, d := range *devices {
		u := usage[d.ID]
		if u == nil {
			u = &deviceUsage{}
		}
		if u.unknown {
			continue
		}
		used := min(u.memory, float64(d.MemoryMiB)*mib)
		utilization := min(u.utilization, 100)
		lv := []string{c.opts.Node, strconv.Itoa(d.Index), d.ID, d.Type}
		gauge(ch, hostMemoryUsed, used, lv...)
		gauge(ch, hostUtilization, utilization, lv...)
		if c.opts.Legacy {
			gauge(ch, legacyHostMemoryUsed, used, lv...)
			gauge(ch, legacyHostUtilization, utilization, lv...)
		}
	}
}

func gauge(ch chan<- prometheus.Metric, d *prometheus.Desc, v float64, labelValues ...string) {
	ch <- prometheus.MustNewConstMetric(d, prometheus.GaugeValue, v, labelValues...)
}
