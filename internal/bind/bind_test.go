// SPDX-FileCopyrightText: 2026 Reza Jelveh
// SPDX-License-Identifier: Apache-2.0

package bind

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	listerscorev1 "k8s.io/client-go/listers/core/v1"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"

	"github.com/fishing-agents/hami-mock-monitor/internal/hami"
)

const node = "gpu-node-1"

func pendingPod(name, phase string, mutate ...func(*corev1.Pod)) *corev1.Pod {
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "team-a", Name: name, UID: types.UID("uid-" + name),
			Labels:      map[string]string{hami.AssignedNodeLabel: node},
			Annotations: map[string]string{hami.BindPhaseAnnotation: phase},
		},
		Spec:   corev1.PodSpec{NodeName: node},
		Status: corev1.PodStatus{Phase: corev1.PodPending},
	}
	for _, m := range mutate {
		m(p)
	}
	return p
}

func lockedNode(lock string) *corev1.Node {
	n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: node, ResourceVersion: "1"}}
	if lock != "" {
		n.Annotations = map[string]string{hami.NodeLockAnnotation: lock}
	}
	return n
}

// setup returns a completer whose pod cache is a snapshot of pods: it does
// not see the completer's own patches, like an informer that lags behind.
func setup(t *testing.T, n *corev1.Node, pods ...*corev1.Pod) (*Completer, *fake.Clientset) {
	t.Helper()
	objects := []runtime.Object{n}
	indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	for _, p := range pods {
		objects = append(objects, p)
		if err := indexer.Add(p.DeepCopy()); err != nil {
			t.Fatal(err)
		}
	}
	client := fake.NewClientset(objects...)
	return New(client, listerscorev1.NewPodLister(indexer), node), client
}

func bindPhase(t *testing.T, client *fake.Clientset, name string) string {
	t.Helper()
	p, err := client.CoreV1().Pods("team-a").Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return p.Annotations[hami.BindPhaseAnnotation]
}

func lock(t *testing.T, client *fake.Clientset) (string, bool) {
	t.Helper()
	n, err := client.CoreV1().Nodes().Get(context.Background(), node, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	v, ok := n.Annotations[hami.NodeLockAnnotation]
	return v, ok
}

func patches(client *fake.Clientset) []k8stesting.PatchAction {
	var out []k8stesting.PatchAction
	for _, a := range client.Actions() {
		if p, ok := a.(k8stesting.PatchAction); ok {
			out = append(out, p)
		}
	}
	return out
}

func TestCompletesOnlyTheLockedPendingPod(t *testing.T) {
	ctx := context.Background()
	c, client := setup(t, lockedNode("2026-10-15T10:00:00Z,team-a,locked"),
		pendingPod("locked", hami.BindPhaseAllocating),
		pendingPod("waiting", hami.BindPhaseAllocating),
	)
	if err := c.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if got := bindPhase(t, client, "locked"); got != hami.BindPhaseSuccess {
		t.Errorf("locked pod: bind phase %q, want success", got)
	}
	if got := bindPhase(t, client, "waiting"); got != hami.BindPhaseAllocating {
		t.Errorf("pod not named by the lock: bind phase %q, want allocating", got)
	}
	if v, held := lock(t, client); held {
		t.Errorf("node lock %q not released", v)
	}

	// Again, with the cache still showing allocating: nothing left to do.
	client.ClearActions()
	if err := c.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if p := patches(client); len(p) != 0 {
		t.Errorf("second reconcile patched %d objects", len(p))
	}
	if got := bindPhase(t, client, "locked"); got != hami.BindPhaseSuccess {
		t.Errorf("second reconcile changed the bind phase to %q", got)
	}
}

func TestReleasesLockOfAlreadyCompletedPod(t *testing.T) {
	// A previous run patched the pod but died before releasing the lock.
	c, client := setup(t, lockedNode("2026-10-15T10:00:00Z,team-a,done"), pendingPod("done", hami.BindPhaseSuccess))
	if err := c.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if v, held := lock(t, client); held {
		t.Errorf("node lock %q not released", v)
	}
	if p := patches(client); len(p) != 1 || p[0].GetResource().Resource != "nodes" {
		t.Errorf("want exactly the node patch, got %d patches", len(p))
	}
}

func TestLeavesOtherPodsAndLocksAlone(t *testing.T) {
	running := func(p *corev1.Pod) { p.Status.Phase = corev1.PodRunning }
	elsewhere := func(p *corev1.Pod) { p.Spec.NodeName = "other-node" }
	for _, tc := range []struct {
		name string
		lock string
		pod  *corev1.Pod
	}{
		{"running pod", "2026-10-15T10:00:00Z,team-a,p", pendingPod("p", hami.BindPhaseAllocating, running)},
		{"pod bound elsewhere", "2026-10-15T10:00:00Z,team-a,p", pendingPod("p", hami.BindPhaseAllocating, elsewhere)},
		{"failed bind", "2026-10-15T10:00:00Z,team-a,p", pendingPod("p", "failed")},
		{"lock names another pod", "2026-10-15T10:00:00Z,team-a,q", pendingPod("p", hami.BindPhaseAllocating)},
		{"lock in another namespace", "2026-10-15T10:00:00Z,team-b,p", pendingPod("p", hami.BindPhaseAllocating)},
		{"legacy lock without owner", "2026-10-15T10:00:00Z", pendingPod("p", hami.BindPhaseAllocating)},
		{"no lock", "", pendingPod("p", hami.BindPhaseAllocating)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, client := setup(t, lockedNode(tc.lock), tc.pod)
			if err := c.Reconcile(context.Background()); err != nil {
				t.Fatal(err)
			}
			if p := patches(client); len(p) != 0 {
				t.Errorf("patched %d objects", len(p))
			}
		})
	}
}

func TestNoCandidateNoAPICalls(t *testing.T) {
	running := pendingPod("p", hami.BindPhaseSuccess, func(p *corev1.Pod) { p.Status.Phase = corev1.PodRunning })
	c, client := setup(t, lockedNode("2026-10-15T10:00:00Z,team-a,p"), running)
	if err := c.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if a := client.Actions(); len(a) != 0 {
		t.Errorf("want no API calls without a pending pod, got %v", a)
	}
}
