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
// not see the completer's own patches until syncCache, like a lagging informer.
func setup(t *testing.T, n *corev1.Node, pods ...*corev1.Pod) (*Completer, *fake.Clientset, cache.Indexer) {
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
	return New(client, listerscorev1.NewPodLister(indexer), node), client, indexer
}

func syncCache(t *testing.T, client *fake.Clientset, indexer cache.Indexer) {
	t.Helper()
	pods, err := client.CoreV1().Pods("").List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for i := range pods.Items {
		if err := indexer.Update(&pods.Items[i]); err != nil {
			t.Fatal(err)
		}
	}
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

func TestCompletesOpenBindsAndReleasesTheirLock(t *testing.T) {
	ctx := context.Background()
	c, client, indexer := setup(t, lockedNode("2026-10-15T10:00:00Z,team-a,locked"),
		pendingPod("locked", hami.BindPhaseAllocating),
		pendingPod("unlocked", hami.BindPhaseAllocating),
	)
	if err := c.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"locked", "unlocked"} {
		if got := bindPhase(t, client, name); got != hami.BindPhaseSuccess {
			t.Errorf("%s: bind phase %q, want success", name, got)
		}
	}
	if v, held := lock(t, client); held {
		t.Errorf("node lock %q not released", v)
	}

	// Again before the cache caught up, then after: same end state, and
	// nothing left to patch once the cache shows it.
	if err := c.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	syncCache(t, client, indexer)
	client.ClearActions()
	if err := c.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if p := patches(client); len(p) != 0 {
		t.Errorf("reconcile after completion patched %d objects", len(p))
	}
	if got := bindPhase(t, client, "locked"); got != hami.BindPhaseSuccess {
		t.Errorf("repeated reconciles changed the bind phase to %q", got)
	}
}

func TestCompletesPodRunningBeforeItWasSeen(t *testing.T) {
	running := pendingPod("p", hami.BindPhaseAllocating, func(p *corev1.Pod) { p.Status.Phase = corev1.PodRunning })
	c, client, _ := setup(t, lockedNode("2026-10-15T10:00:00Z,team-a,p"), running)
	if err := c.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := bindPhase(t, client, "p"); got != hami.BindPhaseSuccess {
		t.Errorf("bind phase %q, want success", got)
	}
	if v, held := lock(t, client); held {
		t.Errorf("node lock %q not released", v)
	}
}

func TestKeepsLockNotNamingTheCompletedPod(t *testing.T) {
	for _, l := range []string{
		"2026-10-15T10:00:00Z,team-a,other", // another pod
		"2026-10-15T10:00:00Z,team-b,p",     // same name, another namespace
		"2026-10-15T10:00:00Z",              // legacy form, names no pod
	} {
		t.Run(l, func(t *testing.T) {
			running := pendingPod("p", hami.BindPhaseAllocating, func(p *corev1.Pod) { p.Status.Phase = corev1.PodRunning })
			c, client, _ := setup(t, lockedNode(l), running)
			if err := c.Reconcile(context.Background()); err != nil {
				t.Fatal(err)
			}
			if got := bindPhase(t, client, "p"); got != hami.BindPhaseSuccess {
				t.Errorf("bind phase %q, want success", got)
			}
			if v, held := lock(t, client); !held || v != l {
				t.Errorf("lock = %q (held %v), want %q kept", v, held, l)
			}
		})
	}
}

func TestReleasesLockOfAlreadyCompletedPod(t *testing.T) {
	// A previous run patched the pod but died before releasing the lock.
	c, client, _ := setup(t, lockedNode("2026-10-15T10:00:00Z,team-a,done"), pendingPod("done", hami.BindPhaseSuccess))
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

func TestReleasesLockNamingRunningSuccessPod(t *testing.T) {
	// The release failed earlier (e.g. a resourceVersion conflict) and the
	// container started meanwhile: the lock is still stale.
	running := pendingPod("done", hami.BindPhaseSuccess, func(p *corev1.Pod) { p.Status.Phase = corev1.PodRunning })
	c, client, _ := setup(t, lockedNode("2026-10-15T10:00:00Z,team-a,done"), running)
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

func TestLeavesOtherPodsAlone(t *testing.T) {
	for name, pod := range map[string]*corev1.Pod{
		"pod bound elsewhere": pendingPod("p", hami.BindPhaseAllocating, func(p *corev1.Pod) { p.Spec.NodeName = "other-node" }),
		"pod not bound yet":   pendingPod("p", hami.BindPhaseAllocating, func(p *corev1.Pod) { p.Spec.NodeName = "" }),
		"finished pod":        pendingPod("p", hami.BindPhaseAllocating, func(p *corev1.Pod) { p.Status.Phase = corev1.PodSucceeded }),
		"failed bind":         pendingPod("p", "failed"),
	} {
		t.Run(name, func(t *testing.T) {
			c, client, _ := setup(t, lockedNode("2026-10-15T10:00:00Z,team-a,p"), pod)
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
	// Neither an open bind nor a completed one on this node.
	failed := pendingPod("p", "failed")
	elsewhere := pendingPod("q", hami.BindPhaseSuccess, func(p *corev1.Pod) { p.Spec.NodeName = "other-node" })
	c, client, _ := setup(t, lockedNode("2026-10-15T10:00:00Z,team-a,q"), failed, elsewhere)
	if err := c.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if a := client.Actions(); len(a) != 0 {
		t.Errorf("want no API calls without a candidate pod, got %v", a)
	}
}
