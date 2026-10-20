// SPDX-FileCopyrightText: 2026 Reza Jelveh
// SPDX-License-Identifier: Apache-2.0

// Package smi prints a monitor snapshot the way nvidia-smi prints a node's
// GPUs: the default table (the layout of driver 580) and the CSV of
// --query-gpu and --query-compute-apps. The header says MOCK. Temperature,
// power and performance state are derived from utilization.
package smi

import (
	"cmp"
	"errors"
	"flag"
	"fmt"
	"hash/fnv"
	"io"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/fishing-agents/hami-mock-monitor/internal/collector"
)

// Version is the driver version the mock reports, a 580 release.
const Version = "580.105.08"

const (
	cudaVersion = "13.0"
	mib         = 1 << 20
	idleTemp    = 30.0
	busyTemp    = 80.0
	idlePower   = 60.0
	powerCap    = 400.0
	// pidMax is Linux's PID_MAX_LIMIT on 64-bit.
	pidMax = 1 << 22
	na     = "N/A"
	csvNA  = "[N/A]"
)

// ErrNoDevices means there is no GPU to show; nvidia-smi then prints
// "No devices were found" and exits 6.
var ErrNoDevices = errors.New("No devices were found")

// ArgError is an invalid argument; nvidia-smi exits 2.
type ArgError string

func (e ArgError) Error() string { return string(e) }

// ErrCombination is nvidia-smi's answer to arguments it cannot use.
const ErrCombination = ArgError("Invalid combination of input arguments. Please run 'nvidia-smi -h' for help.")

// Options are the nvidia-smi arguments both views accept.
type Options struct {
	QueryGPU  string
	QueryApps string
	Format    string
}

// AddFlags registers the options on fs.
func (o *Options) AddFlags(fs *flag.FlagSet) {
	fs.StringVar(&o.QueryGPU, "query-gpu", "", "")
	fs.StringVar(&o.QueryApps, "query-compute-apps", "", "")
	fs.StringVar(&o.Format, "format", "", "")
}

// Usage writes the help; extra lists the options of the calling view.
func Usage(w io.Writer, extra string) {
	fmt.Fprintf(w, `NVIDIA System Management Interface -- v%s MOCK

Mock GPUs of HAMi's mock device plugin, read from hami-mock-monitor; no real
hardware is involved.

Usage: nvidia-smi [OPTION1 [ARG1]] [OPTION2 [ARG2]] ...

    -h,   --help                Print usage information and exit.
%s
    --query-gpu=                Information about GPU, a comma separated list of: index, name,
                                uuid, pci.bus_id, memory.total, memory.used, memory.free,
                                utilization.gpu, temperature.gpu, power.draw, power.limit
    --query-compute-apps=       List of active compute processes, a comma separated list of:
                                pid, process_name, used_memory
    --format=                   csv[,noheader][,nounits]
`, Version, extra)
}

// Run checks the arguments, then writes the table or the CSV for the
// snapshot fetch returns.
func (o Options) Run(w io.Writer, now time.Time, fetch func() (collector.Snapshot, error)) error {
	render, err := o.renderer(now)
	if err != nil {
		return err
	}
	s, err := fetch()
	if err != nil {
		return err
	}
	if len(s.Devices) == 0 {
		return ErrNoDevices
	}
	_, err = io.WriteString(w, render(s))
	return err
}

func (o Options) renderer(now time.Time) (func(collector.Snapshot) string, error) {
	if o.QueryGPU == "" && o.QueryApps == "" {
		if o.Format != "" {
			return nil, ErrCombination
		}
		return func(s collector.Snapshot) string { return Table(s, now) }, nil
	}
	if o.QueryGPU != "" && o.QueryApps != "" {
		return nil, ErrCombination
	}
	header, units, err := parseFormat(o.Format)
	if err != nil {
		return nil, err
	}
	if o.QueryGPU != "" {
		fields, err := parseFields(o.QueryGPU, gpuFields)
		if err != nil {
			return nil, err
		}
		return func(s collector.Snapshot) string {
			return writeCSV(fields, s.Devices, header, units)
		}, nil
	}
	fields, err := parseFields(o.QueryApps, appFields)
	if err != nil {
		return nil, err
	}
	return func(s collector.Snapshot) string { return writeCSV(fields, s.Processes, header, units) }, nil
}

func parseFormat(f string) (header, units bool, err error) {
	header, units = true, true
	csv := false
	for opt := range strings.SplitSeq(f, ",") {
		switch strings.TrimSpace(opt) {
		case "csv":
			csv = true
		case "noheader":
			header = false
		case "nounits":
			units = false
		default:
			return false, false, ErrCombination
		}
	}
	if !csv {
		return false, false, ErrCombination
	}
	return header, units, nil
}

// field is one --query-gpu or --query-compute-apps property.
type field[T any] struct {
	header string
	unit   string
	value  func(T) string
}

func parseFields[T any](q string, known map[string]field[T]) ([]field[T], error) {
	var fields []field[T]
	for name := range strings.SplitSeq(q, ",") {
		name = strings.TrimSpace(name)
		f, ok := known[name]
		if !ok {
			return nil, ArgError(fmt.Sprintf("Field %q is not a valid field to query.", name))
		}
		fields = append(fields, f)
	}
	return fields, nil
}

func writeCSV[T any](fields []field[T], rows []T, header, units bool) string {
	var b strings.Builder
	if header {
		names := make([]string, len(fields))
		for i, f := range fields {
			names[i] = f.header
		}
		b.WriteString(strings.Join(names, ", ") + "\n")
	}
	for _, r := range rows {
		values := make([]string, len(fields))
		for i, f := range fields {
			values[i] = f.value(r)
			if units && f.unit != "" && values[i] != csvNA {
				values[i] += " " + f.unit
			}
		}
		b.WriteString(strings.Join(values, ", ") + "\n")
	}
	return b.String()
}

var (
	gpuFields = map[string]field[collector.Device]{
		"index":        {header: "index", value: func(d collector.Device) string { return strconv.Itoa(d.Index) }},
		"name":         {header: "name", value: func(d collector.Device) string { return d.Type }},
		"uuid":         {header: "uuid", value: func(d collector.Device) string { return d.UUID }},
		"pci.bus_id":   {header: "pci.bus_id", value: func(d collector.Device) string { return busID(d.Index) }},
		"memory.total": {header: "memory.total [MiB]", unit: "MiB", value: func(d collector.Device) string { return strconv.FormatInt(d.MemoryBytes/mib, 10) }},
		"memory.used": {header: "memory.used [MiB]", unit: "MiB", value: func(d collector.Device) string {
			return known(d, func() string { return strconv.FormatInt(usedMiB(d.UsedBytes), 10) })
		}},
		"memory.free": {header: "memory.free [MiB]", unit: "MiB", value: func(d collector.Device) string {
			return known(d, func() string { return strconv.FormatInt(d.MemoryBytes/mib-usedMiB(d.UsedBytes), 10) })
		}},
		"utilization.gpu": {header: "utilization.gpu [%]", unit: "%", value: func(d collector.Device) string {
			return known(d, func() string { return strconv.Itoa(utilization(d)) })
		}},
		"temperature.gpu": {header: "temperature.gpu", value: func(d collector.Device) string {
			return known(d, func() string { return strconv.Itoa(temperature(d)) })
		}},
		"power.draw": {header: "power.draw [W]", unit: "W", value: func(d collector.Device) string {
			return known(d, func() string { return strconv.FormatFloat(power(d), 'f', 2, 64) })
		}},
		"power.limit": {header: "power.limit [W]", unit: "W", value: func(collector.Device) string { return strconv.FormatFloat(powerCap, 'f', 2, 64) }},
	}

	usedMemory = field[collector.Process]{header: "used_gpu_memory [MiB]", unit: "MiB", value: func(p collector.Process) string {
		if p.Unknown {
			return csvNA
		}
		return strconv.FormatInt(usedMiB(p.UsedBytes), 10)
	}}
	appFields = map[string]field[collector.Process]{
		"pid":             {header: "pid", value: func(p collector.Process) string { return strconv.FormatUint(uint64(pid(p)), 10) }},
		"process_name":    {header: "process_name", value: processName},
		"used_memory":     usedMemory,
		"used_gpu_memory": usedMemory,
	}
)

func known(d collector.Device, f func() string) string {
	if d.Unknown {
		return csvNA
	}
	return f()
}

// Table renders the default nvidia-smi output.
func Table(s collector.Snapshot, now time.Time) string {
	const (
		border  = "+-----------------------------------------------------------------------------------------+\n"
		gpuLine = "+-----------------------------------------+------------------------+----------------------+\n"
		gpuHead = "| GPU  Name                 Persistence-M | Bus-Id          Disp.A | Volatile Uncorr. ECC |\n" +
			"| Fan  Temp   Perf          Pwr:Usage/Cap |           Memory-Usage | GPU-Util  Compute M. |\n" +
			"|                                         |                        |               MIG M. |\n" +
			"|=========================================+========================+======================|\n"
		procHead = "| Processes:                                                                              |\n" +
			"|  GPU   GI   CI              PID   Type   Process name                        GPU Memory |\n" +
			"|        ID   ID                                                               Usage      |\n" +
			"|=========================================================================================|\n"
	)
	var b strings.Builder
	b.WriteString(now.Format("Mon Jan _2 15:04:05 2006") + "\n")
	b.WriteString(border)
	fmt.Fprintf(&b, "| NVIDIA-SMI %-23sDriver Version: %-15sCUDA Version: %-9s|\n", Version+" MOCK", Version, cudaVersion)
	b.WriteString(gpuLine)
	b.WriteString(gpuHead)
	index := make(map[string]int, len(s.Devices))
	for _, d := range s.Devices {
		index[d.UUID] = d.Index
		temp, perf, pwr, used, util := na, na, na, na, na
		if !d.Unknown {
			u := utilization(d)
			temp = strconv.Itoa(temperature(d)) + "C"
			perf = "P8"
			if u > 0 {
				perf = "P0"
			}
			pwr = strconv.Itoa(int(math.Round(power(d)))) + "W"
			used = strconv.FormatInt(usedMiB(d.UsedBytes), 10) + "MiB"
			util = strconv.Itoa(u) + "%"
		}
		fmt.Fprintf(&b, "|%4d  %-31s%-3s |   %-16s %3s |%21s |\n", d.Index, deviceName(d.Type), "On", busID(d.Index), "Off", na)
		fmt.Fprintf(&b, "| %3s   %3s%6s%16s / %4.0fW |%11s / %9s |%8s%13s |\n", na, temp, perf, pwr, powerCap,
			used, strconv.FormatInt(d.MemoryBytes/mib, 10)+"MiB", util, "Default")
		fmt.Fprintf(&b, "|%41s|%24s|%21s |\n", "", "", na)
		b.WriteString(gpuLine)
	}
	b.WriteString("\n")
	b.WriteString(border)
	b.WriteString(procHead)
	// Processes on an unregistered device go last.
	gpu := func(p collector.Process) int {
		if i, ok := index[p.DeviceUUID]; ok {
			return i
		}
		return math.MaxInt
	}
	procs := slices.Clone(s.Processes)
	slices.SortStableFunc(procs, func(a, b collector.Process) int { return cmp.Compare(gpu(a), gpu(b)) })
	if len(procs) == 0 {
		fmt.Fprintf(&b, "|  %-87s|\n", "No running processes found")
	}
	for _, p := range procs {
		g := na
		if i := gpu(p); i != math.MaxInt {
			g = strconv.Itoa(i)
		}
		mem := na
		if !p.Unknown {
			mem = strconv.FormatInt(usedMiB(p.UsedBytes), 10) + "MiB"
		}
		fmt.Fprintf(&b, "|%5s   %-3s  %-3s%16d%7s   %-36s %9s | (%dMiB alloc)\n",
			g, na, na, pid(p), "C", truncateLeft(processName(p), 36), mem, p.LimitBytes/mib)
	}
	b.WriteString(border)
	return b.String()
}

func utilization(d collector.Device) int { return int(math.Round(d.Utilization)) }

func temperature(d collector.Device) int {
	return int(math.Round(idleTemp + (busyTemp-idleTemp)*d.Utilization/100))
}

func power(d collector.Device) float64 { return idlePower + (powerCap-idlePower)*d.Utilization/100 }

func usedMiB(b float64) int64 { return int64(b / mib) }

// busID derives a PCI bus id from the device index, bus index+1.
func busID(index int) string { return fmt.Sprintf("00000000:%02X:00.0", index+1) }

// pid is a stable fake process id per container.
func pid(p collector.Process) uint32 {
	h := fnv.New32a()
	h.Write([]byte(p.PodUID + "/" + p.Container))
	return 1 + h.Sum32()%(pidMax-1)
}

func processName(p collector.Process) string { return p.Namespace + "/" + p.Pod + "/" + p.Container }

// deviceName cuts long names as nvidia-smi does.
func deviceName(s string) string {
	if len(s) > 27 {
		return s[:24] + "..."
	}
	return s
}

// truncateLeft keeps the end of a long process name, as nvidia-smi does.
func truncateLeft(s string, n int) string {
	if len(s) > n {
		return "..." + s[len(s)-n+3:]
	}
	return s
}
