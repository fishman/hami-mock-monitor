// SPDX-FileCopyrightText: 2026 Reza Jelveh
// SPDX-License-Identifier: Apache-2.0

// Command hami-mock-monitor stands in for HAMi's vGPUmonitor on nodes whose
// devices come from HAMi's mock device plugin. Run it as a DaemonSet with
// NODE_NAME set; it serves the vGPUmonitor metrics on :9394/metrics.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/fishing-agents/hami-mock-monitor/internal/bind"
	"github.com/fishing-agents/hami-mock-monitor/internal/collector"
	"github.com/fishing-agents/hami-mock-monitor/internal/hami"
	"github.com/fishing-agents/hami-mock-monitor/internal/load"
)

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))
	if err := run(); err != nil {
		slog.Error("exit", "err", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		addr            = flag.String("metrics-bind-address", ":9394", "address to serve /metrics on")
		node            = flag.String("node-name", os.Getenv("NODE_NAME"), "node to monitor (default $NODE_NAME)")
		kubeconfig      = flag.String("kubeconfig", os.Getenv("KUBECONFIG"), "kubeconfig file; in-cluster config when empty (default $KUBECONFIG)")
		legacy          = flag.Bool("legacy-metrics", false, "also emit HAMi's legacy metric names")
		countAllocating = flag.Bool("count-allocating", false, "also count pods whose bind phase is still allocating")
		completeBinds   = flag.Bool("complete-binds", false, "complete binds the mock device plugin leaves open (workaround, see README)")
		bindInterval    = flag.Duration("complete-binds-interval", 2*time.Second, "how often to look for an open bind")
		nodeInterval    = flag.Duration("node-refresh-interval", 30*time.Second, "how often to re-read the node's registered devices")
		scrapeInterval  = flag.Duration("scrape-interval", 15*time.Second, "how often to scrape vllm pods")
		source          = flag.String("load-source", string(load.SourceSeed), "default load source: vllm, seed or static")
		cfg             load.Config
	)
	flag.Int64Var(&cfg.Seed, "seed", 0, "seed source: seed of the curves")
	flag.DurationVar(&cfg.SeedPeriod, "seed-period", 10*time.Minute, "seed source: period of the curves")
	flag.Float64Var(&cfg.SeedMin, "seed-min", 10, "seed source: lower bound in percent of the allocation, for load and memory")
	flag.Float64Var(&cfg.SeedMax, "seed-max", 90, "seed source: upper bound in percent of the allocation, for load and memory")
	flag.Float64Var(&cfg.StaticUtilization, "static-utilization", 50, "static source: percent of the allocated cores in use, 0-100")
	flag.Float64Var(&cfg.StaticMemoryFraction, "static-memory-fraction", 0.5, "static source: share of the memory limit in use, 0-1")
	flag.IntVar(&cfg.VLLMPort, "vllm-port", 8000, "vllm source: port when the pod names none")
	flag.StringVar(&cfg.VLLMPath, "vllm-path", "/metrics", "vllm source: metrics path")
	flag.Float64Var(&cfg.VLLMMaxNumSeqs, "vllm-max-num-seqs", 5, "vllm source: running requests that mean full load")
	flag.Float64Var(&cfg.VLLMMemoryFraction, "vllm-memory-fraction", 0.9, "vllm source: share of the memory limit vLLM pre-allocates (its gpu-memory-utilization), 0-1")
	flag.DurationVar(&cfg.ScrapeTimeout, "scrape-timeout", 5*time.Second, "vllm source: scrape timeout")
	flag.DurationVar(&cfg.StaleAfter, "stale-after", time.Minute, "vllm source: how long a last value outlives failing scrapes")
	flag.Parse()
	cfg.Source = load.Source(*source)

	if *node == "" {
		return errors.New("no node: set NODE_NAME or --node-name")
	}
	if *bindInterval <= 0 || *nodeInterval <= 0 || *scrapeInterval <= 0 {
		return errors.New("intervals must be positive")
	}
	loads, err := load.NewResolver(cfg)
	if err != nil {
		return err
	}
	var restConfig *rest.Config
	if *kubeconfig != "" {
		restConfig, err = clientcmd.BuildConfigFromFlags("", *kubeconfig)
	} else {
		restConfig, err = rest.InClusterConfig()
	}
	if err != nil {
		return fmt.Errorf("kubernetes config: %w", err)
	}
	client, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Only the pods HAMi assigned to this node; the scheduler labels them.
	factory := informers.NewSharedInformerFactoryWithOptions(client, 0, informers.WithTweakListOptions(func(o *metav1.ListOptions) {
		o.LabelSelector = hami.AssignedNodeLabel + "=" + *node
	}))
	podInformer := factory.Core().V1().Pods()
	pods := podInformer.Lister()
	// Bind completion runs on every pod event as well as on its interval, so an
	// open bind is completed as soon as the informer sees it.
	podEvent := make(chan struct{}, 1)
	if *completeBinds {
		poke := func() {
			select {
			case podEvent <- struct{}{}:
			default:
			}
		}
		if _, err := podInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
			AddFunc:    func(any) { poke() },
			UpdateFunc: func(any, any) { poke() },
		}); err != nil {
			return err
		}
	}
	factory.Start(ctx.Done())
	for typ, ok := range factory.WaitForCacheSync(ctx.Done()) {
		if !ok {
			return fmt.Errorf("sync %v cache", typ)
		}
	}

	col := collector.New(collector.Options{Node: *node, CountAllocating: *countAllocating, Legacy: *legacy}, pods, loads)
	reg := prometheus.NewRegistry()
	if err := collector.Register(reg, col); err != nil {
		return err
	}

	go every(ctx, *nodeInterval, nil, func() { refreshNodeDevices(ctx, client, *node, col) })
	go every(ctx, *scrapeInterval, nil, func() { loads.Refresh(ctx, col.Pods(), time.Now()) })
	if *completeBinds {
		completer := bind.New(client, pods, *node)
		go every(ctx, *bindInterval, podEvent, func() {
			if err := completer.Reconcile(ctx); err != nil {
				slog.Warn("complete bind", "err", err)
			}
		})
	}

	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok\n")) })
	listener, err := net.Listen("tcp", *addr)
	if err != nil {
		return err
	}
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 15 * time.Second}
	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve(listener) }()
	slog.Info("serving", "addr", listener.Addr().String(), "node", *node, "load-source", cfg.Source)

	select {
	case err := <-serveErr:
		return err
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return server.Shutdown(shutdown)
}

// every runs f now, then on each tick and each trigger until ctx ends.
func every(ctx context.Context, d time.Duration, trigger <-chan struct{}, f func()) {
	t := time.NewTicker(d)
	defer t.Stop()
	for {
		f()
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-trigger:
		}
	}
}

// refreshNodeDevices keeps the last good device list when the node cannot be
// read, and drops it when the registration is gone or unreadable.
func refreshNodeDevices(ctx context.Context, client kubernetes.Interface, name string, col *collector.Collector) {
	n, err := client.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		slog.Warn("get node", "node", name, "err", err)
		return
	}
	v, ok := n.Annotations[hami.NodeRegisterAnnotation]
	if !ok {
		slog.Warn("node has no device registration, host series off", "node", name, "annotation", hami.NodeRegisterAnnotation)
		col.SetNodeDevices(nil)
		return
	}
	devices, err := hami.DecodeNodeDevices(v)
	if err != nil {
		slog.Warn("undecodable device registration, host series off", "node", name, "err", err)
	}
	col.SetNodeDevices(devices)
}
