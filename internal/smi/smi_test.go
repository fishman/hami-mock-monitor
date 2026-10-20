// SPDX-FileCopyrightText: 2026 Reza Jelveh
// SPDX-License-Identifier: Apache-2.0

package smi

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/fishing-agents/hami-mock-monitor/internal/collector"
)

var update = flag.Bool("update", false, "rewrite the golden files")

var now = time.Date(2026, 10, 16, 9, 30, 0, 0, time.UTC)

// fixture has two devices, the second idle, and three containers on the first.
func fixture(t *testing.T) collector.Snapshot {
	t.Helper()
	b, err := os.ReadFile("testdata/snapshot.json")
	if err != nil {
		t.Fatal(err)
	}
	var s collector.Snapshot
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestGolden(t *testing.T) {
	node := fixture(t)
	ctr, ok := node.Container("team-a", "llm-0", "vllm")
	if !ok {
		t.Fatal("fixture container missing")
	}
	const allGPU = "index,name,uuid,pci.bus_id,memory.total,memory.used,memory.free,utilization.gpu,temperature.gpu,power.draw,power.limit"
	for _, tc := range []struct {
		golden string
		snap   collector.Snapshot
		opts   Options
	}{
		{"node.golden", node, Options{}},
		{"container.golden", ctr, Options{}},
		{"query-gpu.golden", node, Options{QueryGPU: allGPU, Format: "csv"}},
		{"query-gpu-noheader-nounits.golden", node, Options{QueryGPU: allGPU, Format: "csv,noheader,nounits"}},
		{"query-apps.golden", node, Options{QueryApps: "pid,process_name,used_memory", Format: "csv"}},
		{"container-query-apps.golden", ctr, Options{QueryApps: "pid,process_name,used_memory", Format: "csv,nounits"}},
	} {
		t.Run(tc.golden, func(t *testing.T) {
			var out bytes.Buffer
			if err := tc.opts.Run(&out, now, func() (collector.Snapshot, error) { return tc.snap, nil }); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join("testdata", tc.golden)
			if *update {
				if err := os.WriteFile(path, out.Bytes(), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(out.Bytes(), want) {
				t.Errorf("output differs from %s:\n%s", path, out.String())
			}
		})
	}
}

func TestArgumentErrorsBeforeFetch(t *testing.T) {
	for _, tc := range []struct {
		opts Options
		want string
	}{
		{Options{QueryGPU: "index,clocks.sm", Format: "csv"}, `Field "clocks.sm" is not a valid field to query.`},
		{Options{QueryApps: "pid,gpu_uuid", Format: "csv"}, `Field "gpu_uuid" is not a valid field to query.`},
		{Options{QueryGPU: "index"}, string(errCombination)},
		{Options{QueryGPU: "index", Format: "json"}, string(errCombination)},
		{Options{QueryGPU: "index", QueryApps: "pid", Format: "csv"}, string(errCombination)},
		{Options{Format: "csv"}, string(errCombination)},
	} {
		err := tc.opts.Run(&bytes.Buffer{}, now, func() (collector.Snapshot, error) {
			t.Fatal("fetched despite invalid arguments")
			return collector.Snapshot{}, nil
		})
		var argErr ArgError
		if !errors.As(err, &argErr) || err.Error() != tc.want {
			t.Errorf("%+v: got %v, want %q", tc.opts, err, tc.want)
		}
	}
}

func TestNoDevices(t *testing.T) {
	err := Options{}.Run(&bytes.Buffer{}, now, func() (collector.Snapshot, error) { return collector.Snapshot{Node: "n"}, nil })
	if !errors.Is(err, ErrNoDevices) {
		t.Fatalf("got %v, want ErrNoDevices", err)
	}
}
