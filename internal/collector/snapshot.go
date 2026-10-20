// SPDX-FileCopyrightText: 2026 Reza Jelveh
// SPDX-License-Identifier: Apache-2.0

package collector

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"slices"

	corev1 "k8s.io/api/core/v1"

	"github.com/fishing-agents/hami-mock-monitor/internal/hami"
	"github.com/fishing-agents/hami-mock-monitor/internal/load"
)

// Snapshot is the node state the metrics are built from. /smi serves it.
type Snapshot struct {
	Node string `json:"node"`
	// Devices are the registered cards by index, none without a readable
	// registration.
	Devices []Device `json:"devices"`
	// Processes holds one entry per device of each allocated container.
	Processes []Process `json:"processes"`
}

// Device is a registered card, or in a container's slice one of its vGPUs.
type Device struct {
	Index       int     `json:"index"`
	UUID        string  `json:"uuid"`
	Type        string  `json:"type"`
	MemoryBytes int64   `json:"memoryBytes"`
	UsedBytes   float64 `json:"usedBytes"`
	// Utilization is in percent of the whole device.
	Utilization float64 `json:"utilization"`
	// Unknown marks a device with a container whose load is unknown: its
	// sum would understate, so usage is left out.
	Unknown bool `json:"unknown,omitempty"`
}

// PowerCap is every mock device's power limit in watts.
const PowerCap = 400.0

const (
	idleTemperature = 30.0
	busyTemperature = 80.0
	idlePower       = 60.0
)

// Temperature is derived from utilization, 30 C idle to 80 C at full load,
// in whole degrees as NVML reports it.
func (d Device) Temperature() int {
	return int(math.Round(idleTemperature + (busyTemperature-idleTemperature)*d.Utilization/100))
}

// Power is derived from utilization, 60 W idle to PowerCap at full load, in
// watts to the milliwatt as NVML reports it.
func (d Device) Power() float64 {
	return math.Round((idlePower+(PowerCap-idlePower)*d.Utilization/100)*1000) / 1000
}

// Process is one device of one allocated container.
type Process struct {
	Namespace  string `json:"namespace"`
	Pod        string `json:"pod"`
	PodUID     string `json:"podUID"`
	Container  string `json:"container"`
	VDevice    int    `json:"vdeviceIndex"`
	DeviceUUID string `json:"deviceUUID"`
	// DeviceType is the registered type, else the allocated one.
	DeviceType  string  `json:"deviceType"`
	LimitBytes  int64   `json:"limitBytes"`
	UsedBytes   float64 `json:"usedBytes"`
	Utilization float64 `json:"utilization"`
	Unknown     bool    `json:"unknown,omitempty"`
}

// Snapshot reads the allocations of this node's pods and their loads now.
func (c *Collector) Snapshot() Snapshot {
	now := c.now()
	var registered []hami.NodeDevice
	if d := c.devices.Load(); d != nil {
		registered = *d
	}
	types := make(map[string]string, len(registered))
	for _, d := range registered {
		types[d.ID] = d.Type
	}

	s := Snapshot{Node: c.opts.Node}
	usage := map[string]Device{}
	pods := c.Pods()
	slices.SortFunc(pods, func(a, b *corev1.Pod) int {
		return cmp.Or(cmp.Compare(a.Namespace, b.Namespace), cmp.Compare(a.Name, b.Name))
	})
	for _, pod := range pods {
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
				p := Process{
					Namespace: pod.Namespace, Pod: pod.Name, PodUID: string(pod.UID), Container: ctr,
					VDevice: vdev, DeviceUUID: d.UUID, DeviceType: cmp.Or(types[d.UUID], d.Type),
					LimitBytes: d.MemoryMiB * mib,
				}
				l, err := c.loads.Load(pod, ctr, d.UUID, now)
				if err != nil {
					p.Unknown = true
					usage[d.UUID] = Device{Unknown: true}
					if !errors.Is(err, load.ErrNoSample) {
						slog.Warn("skip container with invalid load configuration", "pod", pod.Namespace+"/"+pod.Name, "container", ctr, "err", err)
					}
					s.Processes = append(s.Processes, p)
					continue
				}
				p.UsedBytes = l.MemoryFraction * float64(p.LimitBytes)
				// Like HAMi's monitor, utilization is in percent of the whole
				// device; cores 0 means the allocation may use all of it.
				cores := float64(d.Cores)
				if cores == 0 || cores > 100 {
					cores = 100
				}
				p.Utilization = l.ComputeFraction * cores
				if u := usage[d.UUID]; !u.Unknown {
					usage[d.UUID] = Device{UsedBytes: u.UsedBytes + p.UsedBytes, Utilization: u.Utilization + p.Utilization}
				}
				s.Processes = append(s.Processes, p)
			}
		}
	}

	for _, d := range registered {
		u := usage[d.ID]
		dev := Device{Index: d.Index, UUID: d.ID, Type: d.Type, MemoryBytes: d.MemoryMiB * mib, Unknown: u.Unknown}
		if !u.Unknown {
			dev.UsedBytes = min(u.UsedBytes, float64(dev.MemoryBytes))
			dev.Utilization = min(u.Utilization, 100)
		}
		s.Devices = append(s.Devices, dev)
	}
	slices.SortStableFunc(s.Devices, func(a, b Device) int { return cmp.Compare(a.Index, b.Index) })
	return s
}

// Container returns a container's slice as HAMi's libvgpu shows it inside the
// container: one device per vGPU numbered from 0, its memory limit as total,
// its own usage and share. ok is false without an allocation on this node.
func (s Snapshot) Container(namespace, pod, container string) (slice Snapshot, ok bool) {
	slice.Node = s.Node
	for _, p := range s.Processes {
		if p.Namespace != namespace || p.Pod != pod || p.Container != container {
			continue
		}
		slice.Devices = append(slice.Devices, Device{
			Index: p.VDevice, UUID: p.DeviceUUID, Type: p.DeviceType, MemoryBytes: p.LimitBytes,
			UsedBytes: p.UsedBytes, Utilization: p.Utilization, Unknown: p.Unknown,
		})
		slice.Processes = append(slice.Processes, p)
	}
	return slice, len(slice.Processes) > 0
}

// ServeSMI serves the snapshot as JSON. With namespace, pod and container it
// serves only that container's slice, 404 when it has none.
func (c *Collector) ServeSMI(w http.ResponseWriter, r *http.Request) {
	s := c.Snapshot()
	q := r.URL.Query()
	if ns, pod, ctr := q.Get("namespace"), q.Get("pod"), q.Get("container"); ns != "" || pod != "" || ctr != "" {
		if ns == "" || pod == "" || ctr == "" {
			http.Error(w, "namespace, pod and container go together", http.StatusBadRequest)
			return
		}
		var ok bool
		if s, ok = s.Container(ns, pod, ctr); !ok {
			http.Error(w, fmt.Sprintf("no allocation for container %s/%s/%s on node %s", ns, pod, ctr, s.Node), http.StatusNotFound)
			return
		}
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(s); err != nil {
		slog.Warn("write /smi", "err", err)
	}
}
