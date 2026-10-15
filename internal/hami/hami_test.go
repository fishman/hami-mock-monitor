// SPDX-FileCopyrightText: 2026 Reza Jelveh
// SPDX-License-Identifier: Apache-2.0

package hami

import (
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func TestDecodePodDevicesMultiContainerMultiDevice(t *testing.T) {
	// An init container without devices, an app container with two devices,
	// one with one device (the 5th "slots" field is optional), then HAMi's
	// trailing separator.
	got, err := DecodePodDevices(";GPU-a,NVIDIA,40960,30:GPU-b,NVIDIA,20480,0:;GPU-c,NVIDIA,1024,10,3:;")
	if err != nil {
		t.Fatal(err)
	}
	want := [][]ContainerDevice{
		nil,
		{{UUID: "GPU-a", MemoryMiB: 40960, Cores: 30}, {UUID: "GPU-b", MemoryMiB: 20480}},
		{{UUID: "GPU-c", MemoryMiB: 1024, Cores: 10}},
		nil,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestDecodePodDevicesRejectsMalformed(t *testing.T) {
	for _, in := range []string{
		"GPU-a,NVIDIA,40960:;",    // missing cores
		",NVIDIA,40960,0:;",       // missing uuid
		"GPU-a,NVIDIA,lots,0:;",   // memory not a number
		"GPU-a,NVIDIA,-1,0:;",     // negative memory
		"GPU-a,NVIDIA,40960,-5:;", // negative cores
		"GPU-a:;",                 // no fields at all
	} {
		if _, err := DecodePodDevices(in); err == nil {
			t.Errorf("DecodePodDevices(%q) accepted malformed input", in)
		}
	}
}

func TestContainerNameOrdersInitContainersFirst(t *testing.T) {
	pod := &corev1.Pod{Spec: corev1.PodSpec{
		InitContainers: []corev1.Container{{Name: "init"}},
		Containers:     []corev1.Container{{Name: "app"}, {Name: "sidecar"}},
	}}
	for i, want := range []string{"init", "app", "sidecar"} {
		if got, ok := ContainerName(pod, i); !ok || got != want {
			t.Errorf("ContainerName(%d) = %q, %v; want %q", i, got, ok, want)
		}
	}
	if _, ok := ContainerName(pod, 3); ok {
		t.Error("index past the last container resolved")
	}
}

func TestDecodeNodeDevices(t *testing.T) {
	// The index field is omitempty in HAMi, so registrations that leave it out
	// for every card get positional indices; a bare model gets the NVIDIA- prefix.
	got, err := DecodeNodeDevices(`[{"id":"GPU-0","count":10,"devmem":81920,"devcore":100,"type":"A100-SXM4-80GB","health":true},` +
		`{"id":"GPU-1","count":10,"devmem":40960,"devcore":100,"type":"NVIDIA-A100","health":true}]`)
	if err != nil {
		t.Fatal(err)
	}
	want := []NodeDevice{
		{ID: "GPU-0", Index: 0, MemoryMiB: 81920, Type: "NVIDIA-A100-SXM4-80GB"},
		{ID: "GPU-1", Index: 1, MemoryMiB: 40960, Type: "NVIDIA-A100"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}

	got, err = DecodeNodeDevices(`[{"id":"GPU-0","index":1,"devmem":1,"type":"NVIDIA-T4"},{"id":"GPU-1","devmem":1,"type":"NVIDIA-T4"}]`)
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Index != 1 || got[1].Index != 0 {
		t.Fatalf("explicit distinct indices not kept: %+v", got)
	}

	if _, err := DecodeNodeDevices("GPU-0,10,81920,100,NVIDIA-A100,0,true:"); err == nil {
		t.Error("legacy CSV registration accepted")
	}
}

func TestParseNodeLock(t *testing.T) {
	ns, name, ok := ParseNodeLock("2026-10-15T10:00:00Z,team-a,llm-0")
	if !ok || ns != "team-a" || name != "llm-0" {
		t.Fatalf("got %q %q %v", ns, name, ok)
	}
	for _, v := range []string{"2026-10-15T10:00:00Z", "2026-10-15T10:00:00Z,,", "a,b", "a,b,c,d"} {
		if _, _, ok := ParseNodeLock(v); ok {
			t.Errorf("ParseNodeLock(%q) named a pod", v)
		}
	}
}
