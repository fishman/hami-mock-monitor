// SPDX-FileCopyrightText: 2026 Reza Jelveh
// SPDX-License-Identifier: Apache-2.0

// Package hami decodes the HAMi conventions the monitor reads: the pod and node
// annotations HAMi's scheduler and NVIDIA device plugin write, and the node lock.
// Formats follow HAMi v2.10.
package hami

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
)

const (
	// AssignedNodeLabel names the node HAMi assigned a pod to. The scheduler
	// writes it as both label and annotation.
	AssignedNodeLabel = "hami.io/vgpu-node"

	BindPhaseAnnotation = "hami.io/bind-phase"
	BindPhaseAllocating = "allocating"
	BindPhaseSuccess    = "success"

	// DevicesAllocatedAnnotation holds a pod's NVIDIA allocation:
	// "uuid,type,mem_mib,cores[,slots]:" per device, ";" after each container.
	DevicesAllocatedAnnotation = "hami.io/vgpu-devices-allocated"

	// NodeRegisterAnnotation holds a node's NVIDIA cards as a JSON list.
	NodeRegisterAnnotation = "hami.io/node-nvidia-register"

	// NodeLockAnnotation is the node lock the scheduler takes in Bind:
	// "RFC3339,namespace,name" (a bare timestamp in the legacy form).
	NodeLockAnnotation = "hami.io/mutex.lock"
)

// ContainerDevice is one device of one container's allocation.
type ContainerDevice struct {
	UUID      string
	MemoryMiB int64
	Cores     int64
}

// DecodePodDevices decodes DevicesAllocatedAnnotation into one entry per
// container segment. Empty segments are kept: the position is the container
// index (see ContainerName).
func DecodePodDevices(s string) ([][]ContainerDevice, error) {
	var pod [][]ContainerDevice
	for segment := range strings.SplitSeq(s, ";") {
		var ctr []ContainerDevice
		for dev := range strings.SplitSeq(segment, ":") {
			if dev == "" {
				continue
			}
			f := strings.Split(dev, ",")
			if len(f) < 4 || f[0] == "" {
				return nil, fmt.Errorf("malformed device %q", dev)
			}
			mem, err := strconv.ParseInt(f[2], 10, 32)
			if err != nil || mem < 0 {
				return nil, fmt.Errorf("device %q: invalid memory %q", dev, f[2])
			}
			cores, err := strconv.ParseInt(f[3], 10, 32)
			if err != nil || cores < 0 {
				return nil, fmt.Errorf("device %q: invalid cores %q", dev, f[3])
			}
			ctr = append(ctr, ContainerDevice{UUID: f[0], MemoryMiB: mem, Cores: cores})
		}
		pod = append(pod, ctr)
	}
	return pod, nil
}

// ContainerName resolves an allocation index: HAMi orders init containers
// before app containers.
func ContainerName(pod *corev1.Pod, i int) (string, bool) {
	if i < 0 {
		return "", false
	}
	if i < len(pod.Spec.InitContainers) {
		return pod.Spec.InitContainers[i].Name, true
	}
	i -= len(pod.Spec.InitContainers)
	if i >= len(pod.Spec.Containers) {
		return "", false
	}
	return pod.Spec.Containers[i].Name, true
}

// NodeDevice is one registered card.
type NodeDevice struct {
	ID        string
	Index     int
	MemoryMiB int64
	// Type is the device_type label value: the model, NVIDIA-prefixed.
	Type string
}

// DecodeNodeDevices decodes NodeRegisterAnnotation. HAMi omits a zero index,
// so when the registered indices are not distinct, list positions are used.
func DecodeNodeDevices(s string) ([]NodeDevice, error) {
	var raw []struct {
		ID     string `json:"id"`
		Index  int    `json:"index"`
		Devmem int64  `json:"devmem"`
		Type   string `json:"type"`
	}
	if err := json.Unmarshal([]byte(s), &raw); err != nil {
		return nil, fmt.Errorf("decode %s: %w", NodeRegisterAnnotation, err)
	}
	seen := make(map[int]bool, len(raw))
	for _, d := range raw {
		seen[d.Index] = true
	}
	distinct := len(seen) == len(raw)
	devices := make([]NodeDevice, len(raw))
	for i, d := range raw {
		if d.ID == "" {
			return nil, fmt.Errorf("decode %s: device %d has no id", NodeRegisterAnnotation, i)
		}
		index := d.Index
		if !distinct {
			index = i
		}
		devices[i] = NodeDevice{ID: d.ID, Index: index, MemoryMiB: d.Devmem, Type: normalizeModel(d.Type)}
	}
	return devices, nil
}

// normalizeModel mirrors HAMi's device_type labelling: NVML names come with
// and without the vendor prefix, only unprefixed ones get it.
func normalizeModel(model string) string {
	if strings.HasPrefix(model, "NVIDIA") {
		return model
	}
	return "NVIDIA-" + model
}

// ParseNodeLock returns the pod a node lock value names. ok is false for the
// legacy timestamp-only form and anything malformed.
func ParseNodeLock(v string) (namespace, name string, ok bool) {
	f := strings.Split(v, ",")
	if len(f) != 3 || f[1] == "" || f[2] == "" {
		return "", "", false
	}
	return f[1], f[2], true
}
