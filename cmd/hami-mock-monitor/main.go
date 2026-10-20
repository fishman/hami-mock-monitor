// SPDX-FileCopyrightText: 2026 Reza Jelveh
// SPDX-License-Identifier: Apache-2.0

// Command hami-mock-monitor stands in for HAMi's vGPUmonitor on nodes whose
// devices come from HAMi's mock device plugin. Run it as a DaemonSet with
// NODE_NAME set; it serves the vGPUmonitor metrics on :9394/metrics.
//
// "hami-mock-monitor smi" prints the node's mock GPUs as nvidia-smi does;
// called as nvidia-smi it prints the slice of the container it runs in;
// "hami-mock-monitor install-smi DEST" copies itself to DEST for that.
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/fishing-agents/hami-mock-monitor/internal/bind"
	"github.com/fishing-agents/hami-mock-monitor/internal/collector"
	"github.com/fishing-agents/hami-mock-monitor/internal/hami"
	"github.com/fishing-agents/hami-mock-monitor/internal/load"
	"github.com/fishing-agents/hami-mock-monitor/internal/webhook"
)

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))
	if filepath.Base(os.Args[0]) == shimName {
		os.Exit(runSMI(os.Args[1:], false, os.Stdout, os.Stderr))
	}
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "smi":
			os.Exit(runSMI(os.Args[2:], true, os.Stdout, os.Stderr))
		case "install-smi":
			if len(os.Args) != 3 {
				fmt.Fprintln(os.Stderr, "usage: hami-mock-monitor install-smi DEST")
				os.Exit(2)
			}
			if err := installSMI(os.Args[2]); err != nil {
				slog.Error("install nvidia-smi", "err", err)
				os.Exit(1)
			}
			return
		}
	}
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
		bindInterval    = flag.Duration("complete-binds-interval", 2*time.Second, "how often to look for an open bind besides pod and node events")
		scrapeInterval  = flag.Duration("scrape-interval", 15*time.Second, "how often to scrape vllm pods")
		source          = flag.String("load-source", string(load.SourceSeed), "default load source: vllm, seed or static")
		webhookAddr     = flag.String("webhook-bind-address", "", "address to serve the nvidia-smi injection webhook on, HTTPS at /inject; off when empty")
		webhookCertDir  = flag.String("webhook-cert-dir", "/tls", "webhook: directory with tls.crt and tls.key, read on every handshake")
		smiImage        = flag.String("smi-image", "", "webhook: image of the init container copying nvidia-smi, this monitor's image")
		smiURL          = flag.String("smi-url", "", "webhook: HAMI_MOCK_SMI_URL of injected containers, a Service with internalTrafficPolicy Local")
		smiResources    = flag.String("smi-resources", joinResources(webhook.DefaultResources), "webhook: resource limits that mark a new pod as HAMi-allocated, comma separated")
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
	flag.Usage = func() {
		fmt.Fprint(flag.CommandLine.Output(), "Usage: hami-mock-monitor [flags]\n"+
			"       hami-mock-monitor smi [--node NODE | --monitor-url URL] [nvidia-smi options]\n"+
			"       hami-mock-monitor install-smi DEST\n\nFlags:\n")
		flag.PrintDefaults()
	}
	flag.Parse()
	cfg.Source = load.Source(*source)

	if *node == "" {
		return errors.New("no node: set NODE_NAME or --node-name")
	}
	if *bindInterval <= 0 || *scrapeInterval <= 0 {
		return errors.New("intervals must be positive")
	}
	// Staleness counts from the last good sample, so a shorter window would
	// drop vllm series between two successful scrapes.
	if cfg.StaleAfter < *scrapeInterval {
		return fmt.Errorf("--stale-after (%s) must be at least --scrape-interval (%s)", cfg.StaleAfter, *scrapeInterval)
	}
	if *webhookAddr != "" && (*smiImage == "" || *smiURL == "") {
		return errors.New("--webhook-bind-address needs --smi-image and --smi-url")
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
	podFactory := informers.NewSharedInformerFactoryWithOptions(client, 0, informers.WithTweakListOptions(func(o *metav1.ListOptions) {
		o.LabelSelector = hami.AssignedNodeLabel + "=" + *node
	}))
	// Only this node: its device registration and the HAMi node lock.
	nodeFactory := informers.NewSharedInformerFactoryWithOptions(client, 0, informers.WithTweakListOptions(func(o *metav1.ListOptions) {
		o.FieldSelector = fields.OneTermEqualSelector("metadata.name", *node).String()
	}))
	podInformer := podFactory.Core().V1().Pods()
	nodeInformer := nodeFactory.Core().V1().Nodes()
	pods := podInformer.Lister()

	col := collector.New(collector.Options{Node: *node, CountAllocating: *countAllocating, Legacy: *legacy}, pods, loads)
	if _, err := nodeInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj any) { setNodeDevices(col, obj.(*corev1.Node)) },
		UpdateFunc: func(old, cur any) {
			if old.(*corev1.Node).Annotations[hami.NodeRegisterAnnotation] != cur.(*corev1.Node).Annotations[hami.NodeRegisterAnnotation] {
				setNodeDevices(col, cur.(*corev1.Node))
			}
		},
		DeleteFunc: func(any) { col.SetNodeDevices(nil) },
	}); err != nil {
		return err
	}
	var completer *bind.Completer
	if *completeBinds {
		completer = bind.New(client, pods, nodeInformer.Lister(), *node)
		if err := completer.Watch(podInformer.Informer(), nodeInformer.Informer()); err != nil {
			return err
		}
	}
	for _, f := range []informers.SharedInformerFactory{podFactory, nodeFactory} {
		f.Start(ctx.Done())
		for typ, ok := range f.WaitForCacheSync(ctx.Done()) {
			if !ok {
				return fmt.Errorf("sync %v cache", typ)
			}
		}
	}

	reg := prometheus.NewRegistry()
	if err := collector.Register(reg, col); err != nil {
		return err
	}

	go every(ctx, *scrapeInterval, func() { loads.Refresh(ctx, col.Pods(), time.Now()) })
	if completer != nil {
		go completer.Run(ctx, *bindInterval)
	}

	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	mux.HandleFunc("/smi", col.ServeSMI)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok\n")) })
	listener, err := net.Listen("tcp", *addr)
	if err != nil {
		return err
	}
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 15 * time.Second}
	servers := []*http.Server{server}
	serveErr := make(chan error, 2)
	go func() { serveErr <- server.Serve(listener) }()
	slog.Info("serving", "addr", listener.Addr(), "node", *node, "load-source", cfg.Source)
	if *webhookAddr != "" {
		var resources []corev1.ResourceName
		for r := range strings.SplitSeq(*smiResources, ",") {
			resources = append(resources, corev1.ResourceName(strings.TrimSpace(r)))
		}
		injector := webhook.Injector{Image: *smiImage, URL: *smiURL, Resources: resources}
		ws, wl, err := webhookServer(*webhookAddr, *webhookCertDir, injector)
		if err != nil {
			return err
		}
		servers = append(servers, ws)
		go func() { serveErr <- ws.ServeTLS(wl, "", "") }()
		slog.Info("serving webhook", "addr", wl.Addr())
	}

	select {
	case err := <-serveErr:
		return err
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var errs []error
	for _, s := range servers {
		errs = append(errs, s.Shutdown(shutdown))
	}
	return errors.Join(errs...)
}

// webhookServer serves the injector over TLS with the key pair in dir, read
// on every handshake so a renewed certificate needs no restart.
func webhookServer(addr, dir string, injector webhook.Injector) (*http.Server, net.Listener, error) {
	certFile, keyFile := filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key")
	if _, err := tls.LoadX509KeyPair(certFile, keyFile); err != nil {
		return nil, nil, fmt.Errorf("webhook certificate: %w", err)
	}
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, nil, err
	}
	mux := http.NewServeMux()
	mux.Handle("POST /inject", injector)
	return &http.Server{Handler: mux, ReadHeaderTimeout: 15 * time.Second, TLSConfig: &tls.Config{
		MinVersion: tls.VersionTLS12,
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
			c, err := tls.LoadX509KeyPair(certFile, keyFile)
			return &c, err
		},
	}}, l, nil
}

func joinResources(rs []corev1.ResourceName) string {
	s := make([]string, len(rs))
	for i, r := range rs {
		s[i] = string(r)
	}
	return strings.Join(s, ",")
}

// every runs f now, then on each tick until ctx ends.
func every(ctx context.Context, d time.Duration, f func()) {
	t := time.NewTicker(d)
	defer t.Stop()
	for {
		f()
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// setNodeDevices reads the node's device registration; without a readable
// one the host series are off.
func setNodeDevices(col *collector.Collector, n *corev1.Node) {
	v, ok := n.Annotations[hami.NodeRegisterAnnotation]
	if !ok {
		slog.Warn("node has no device registration, host series off", "node", n.Name, "annotation", hami.NodeRegisterAnnotation)
		col.SetNodeDevices(nil)
		return
	}
	devices, err := hami.DecodeNodeDevices(v)
	if err != nil {
		slog.Warn("undecodable device registration, host series off", "node", n.Name, "err", err)
	}
	col.SetNodeDevices(devices)
}
