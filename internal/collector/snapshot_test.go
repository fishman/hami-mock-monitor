// SPDX-FileCopyrightText: 2026 Reza Jelveh
// SPDX-License-Identifier: Apache-2.0

package collector

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/fishing-agents/hami-mock-monitor/internal/load"
)

func getSMI(t *testing.T, c *Collector, query string) (*httptest.ResponseRecorder, Snapshot) {
	t.Helper()
	rec := httptest.NewRecorder()
	c.ServeSMI(rec, httptest.NewRequest(http.MethodGet, "/smi"+query, nil))
	var s Snapshot
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &s); err != nil {
			t.Fatal(err)
		}
	}
	return rec, s
}

func TestSMINodeAndContainerSlice(t *testing.T) {
	const mib = 1 << 20
	multi := newPod("multi", "GPU-0,NVIDIA,512,30:GPU-1,NVIDIA,256,0:;GPU-0,NVIDIA,128,10:;", annotate(load.AnnotationSource, "static"), func(p *corev1.Pod) {
		p.Spec.Containers = []corev1.Container{{Name: "main"}, {Name: "side"}}
	})
	c, _ := newCollector(t, Options{}, resolver(t, load.SourceStatic), multi)

	rec, node := getSMI(t, c, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("node: %d %s", rec.Code, rec.Body)
	}
	if node.Node != "gpu-node-1" || len(node.Devices) != 3 || len(node.Processes) != 3 {
		t.Fatalf("node snapshot: %+v", node)
	}
	// Static load: half the memory limit and half the cores of each device.
	gpu0 := node.Devices[0]
	if gpu0.UUID != "GPU-0" || gpu0.MemoryBytes != 1024*mib || gpu0.UsedBytes != (256+64)*mib || gpu0.Utilization != 15+5 {
		t.Errorf("GPU-0 = %+v, want the sum of main and side", gpu0)
	}
	if idle := node.Devices[2]; idle.UsedBytes != 0 || idle.Utilization != 0 || idle.Unknown {
		t.Errorf("GPU-2 = %+v, want idle", idle)
	}

	rec, slice := getSMI(t, c, "?namespace=team-a&pod=multi&container=main")
	if rec.Code != http.StatusOK {
		t.Fatalf("slice: %d %s", rec.Code, rec.Body)
	}
	want := []Device{
		{Index: 0, UUID: "GPU-0", Type: "NVIDIA-A100", MemoryBytes: 512 * mib, UsedBytes: 256 * mib, Utilization: 15},
		{Index: 1, UUID: "GPU-1", Type: "NVIDIA-A100", MemoryBytes: 256 * mib, UsedBytes: 128 * mib, Utilization: 50},
	}
	if len(slice.Devices) != len(want) || slice.Devices[0] != want[0] || slice.Devices[1] != want[1] {
		t.Errorf("slice devices = %+v, want %+v", slice.Devices, want)
	}
	if len(slice.Processes) != 2 || slice.Processes[0].Container != "main" || slice.Processes[1].Container != "main" {
		t.Errorf("slice processes = %+v, want main's two", slice.Processes)
	}

	for query, code := range map[string]int{
		"?namespace=team-a&pod=multi&container=gone": http.StatusNotFound,
		"?namespace=team-a&pod=other&container=main": http.StatusNotFound,
		"?namespace=team-a&pod=multi":                http.StatusBadRequest,
	} {
		if rec, _ := getSMI(t, c, query); rec.Code != code {
			t.Errorf("%s: %d, want %d", query, rec.Code, code)
		}
	}
}
