// SPDX-FileCopyrightText: 2026 Reza Jelveh
// SPDX-License-Identifier: Apache-2.0

// Package collector serves HAMi's vGPUmonitor metric contract (v2.10) for the
// pods HAMi bound to this node, from their allocation annotations and a load
// source instead of HAMi-core's shared memory and NVML.
package collector

import (
	"log/slog"
	"slices"
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
	return slices.DeleteFunc(pods, func(p *corev1.Pod) bool {
		if p.Spec.NodeName != c.opts.Node || p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
			return true
		}
		switch p.Annotations[hami.BindPhaseAnnotation] {
		case hami.BindPhaseSuccess:
			return false
		case hami.BindPhaseAllocating:
			return !c.opts.CountAllocating
		}
		return true
	})
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

func (c *Collector) Collect(ch chan<- prometheus.Metric) {
	s := c.Snapshot()
	for _, p := range s.Processes {
		if p.Unknown {
			continue
		}
		limit := float64(p.LimitBytes)
		lv := []string{p.Namespace, p.Pod, p.Container, strconv.Itoa(p.VDevice), p.DeviceUUID}
		gauge(ch, vgpuMemoryUsed, p.UsedBytes, lv...)
		gauge(ch, vgpuMemoryLimit, limit, lv...)
		gauge(ch, ctrMemory, p.UsedBytes, lv...)
		gauge(ch, ctrUtilization, p.Utilization, lv...)
		if c.opts.Legacy {
			gauge(ch, legacyVGPUMemoryUsed, p.UsedBytes, lv...)
			gauge(ch, legacyVGPUMemoryLimit, limit, lv...)
			// context, module and data sizes are 0, so all of it is offset.
			gauge(ch, legacyCtrMemory, p.UsedBytes, append(lv, "0", "0", "0", strconv.FormatFloat(p.UsedBytes, 'f', 0, 64))...)
			gauge(ch, legacyCtrUtilization, p.Utilization, lv...)
		}
	}
	for _, d := range s.Devices {
		if d.Unknown {
			continue
		}
		lv := []string{s.Node, strconv.Itoa(d.Index), d.UUID, d.Type}
		gauge(ch, hostMemoryUsed, d.UsedBytes, lv...)
		gauge(ch, hostUtilization, d.Utilization, lv...)
		if c.opts.Legacy {
			gauge(ch, legacyHostMemoryUsed, d.UsedBytes, lv...)
			gauge(ch, legacyHostUtilization, d.Utilization, lv...)
		}
	}
}

func gauge(ch chan<- prometheus.Metric, d *prometheus.Desc, v float64, labelValues ...string) {
	ch <- prometheus.MustNewConstMetric(d, prometheus.GaugeValue, v, labelValues...)
}
