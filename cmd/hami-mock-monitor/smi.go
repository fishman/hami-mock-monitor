// SPDX-FileCopyrightText: 2026 Reza Jelveh
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/fishing-agents/hami-mock-monitor/internal/collector"
	"github.com/fishing-agents/hami-mock-monitor/internal/smi"
)

// shimName is the argv[0] that selects the container view.
const shimName = "nvidia-smi"

const nodeUsage = `
    --node=                     Show the GPUs of this node: asks its hami-mock-monitor pod
                                (app.kubernetes.io/name=hami-mock-monitor) through the API
                                server's pod proxy, with the kubeconfig or in-cluster config.
    --monitor-url=              Monitor to ask, default http://localhost:9394 (in the monitor pod).
`

var errNotFound = errors.New("not found")

// runSMI runs the node view (the smi subcommand) or the container view
// (the binary called as nvidia-smi in an injected container) and returns the
// exit code, nvidia-smi's where it has one.
func runSMI(args []string, nodeView bool, stdout, stderr io.Writer) int {
	prog := shimName
	if nodeView {
		prog = "hami-mock-monitor smi"
	}
	fs := flag.NewFlagSet(prog, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var opts smi.Options
	opts.AddFlags(fs)
	var node, monitorURL, extra string
	if nodeView {
		fs.StringVar(&node, "node", "", "")
		fs.StringVar(&monitorURL, "monitor-url", "http://localhost:9394", "")
		extra = nodeUsage
	}
	if err := fs.Parse(args); err != nil || fs.NArg() > 0 {
		if errors.Is(err, flag.ErrHelp) {
			smi.Usage(stdout, extra)
			return 0
		}
		fmt.Fprintln(stderr, smi.ErrCombination)
		return 2
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	fetch := func() (collector.Snapshot, error) { return containerSnapshot(ctx) }
	if nodeView {
		fetch = func() (collector.Snapshot, error) {
			if node != "" {
				return nodeSnapshot(ctx, node)
			}
			return getSnapshot(ctx, strings.TrimSuffix(monitorURL, "/")+"/smi")
		}
	}
	var argErr smi.ArgError
	switch err := opts.Run(stdout, time.Now(), fetch); {
	case err == nil:
		return 0
	case errors.Is(err, smi.ErrNoDevices):
		fmt.Fprintln(stdout, err)
		return 6
	case errors.As(err, &argErr):
		fmt.Fprintln(stderr, err)
		return 2
	default:
		fmt.Fprintf(stderr, "%s: %v\n", prog, err)
		return 9
	}
}

// containerSnapshot asks the node's monitor for this container's slice, with
// the environment the injection webhook sets.
func containerSnapshot(ctx context.Context) (collector.Snapshot, error) {
	base, ns, pod, ctr := os.Getenv("HAMI_MOCK_SMI_URL"), os.Getenv("POD_NAMESPACE"), os.Getenv("POD_NAME"), os.Getenv("HAMI_MOCK_SMI_CONTAINER")
	if base == "" || ns == "" || pod == "" || ctr == "" {
		return collector.Snapshot{}, errors.New("not a hami-mock-monitor container: HAMI_MOCK_SMI_URL, POD_NAMESPACE, POD_NAME and HAMI_MOCK_SMI_CONTAINER must be set")
	}
	q := url.Values{"namespace": {ns}, "pod": {pod}, "container": {ctr}}
	s, err := getSnapshot(ctx, strings.TrimSuffix(base, "/")+"/smi?"+q.Encode())
	if errors.Is(err, errNotFound) {
		return s, smi.ErrNoDevices
	}
	return s, err
}

func getSnapshot(ctx context.Context, u string) (collector.Snapshot, error) {
	var s collector.Snapshot
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return s, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return s, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return s, fmt.Errorf("%s: %w", u, errNotFound)
	default:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return s, fmt.Errorf("%s: %s: %s", u, resp.Status, strings.TrimSpace(string(body)))
	}
	err = json.NewDecoder(resp.Body).Decode(&s)
	return s, err
}

// nodeSnapshot asks the monitor pod running on node through the API server.
func nodeSnapshot(ctx context.Context, node string) (collector.Snapshot, error) {
	var s collector.Snapshot
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(clientcmd.NewDefaultClientConfigLoadingRules(), &clientcmd.ConfigOverrides{}).ClientConfig()
	if err != nil {
		return s, fmt.Errorf("kubernetes config: %w", err)
	}
	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return s, err
	}
	pods, err := client.CoreV1().Pods("").List(ctx, metav1.ListOptions{
		LabelSelector: "app.kubernetes.io/name=hami-mock-monitor",
		FieldSelector: fields.AndSelectors(
			fields.OneTermEqualSelector("spec.nodeName", node),
			fields.OneTermEqualSelector("status.phase", "Running"),
		).String(),
	})
	if err != nil {
		return s, err
	}
	if len(pods.Items) == 0 {
		return s, fmt.Errorf("no running hami-mock-monitor pod on node %s", node)
	}
	p := pods.Items[0]
	port := "9394"
	for _, c := range p.Spec.Containers {
		for _, cp := range c.Ports {
			if cp.Name == "metrics" {
				port = strconv.Itoa(int(cp.ContainerPort))
			}
		}
	}
	b, err := client.CoreV1().Pods(p.Namespace).ProxyGet("http", p.Name, port, "smi", nil).DoRaw(ctx)
	if err != nil {
		return s, fmt.Errorf("pod %s/%s: %w", p.Namespace, p.Name, err)
	}
	err = json.Unmarshal(b, &s)
	return s, err
}

// installSMI copies this binary to dest, for the injected init container: the
// image has no shell or cp.
func installSMI(dest string) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	in, err := os.Open(self)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
