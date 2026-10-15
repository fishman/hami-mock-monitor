// SPDX-FileCopyrightText: 2026 Reza Jelveh
// SPDX-License-Identifier: Apache-2.0

// Package bind completes the HAMi binds that HAMi's mock device plugin leaves
// open. HAMi's scheduler marks a pod hami.io/bind-phase=allocating and takes
// the node lock in Bind; the real NVIDIA device plugin's Allocate then marks the
// pod success and releases the lock. The mock plugin's Allocate does neither, so
// the pod stays allocating and the next pod waits for the lock to expire.
//
// This is a workaround until the mock plugin completes binds itself (see
// upstream/mock-device-plugin).
package bind

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	listerscorev1 "k8s.io/client-go/listers/core/v1"

	"github.com/fishing-agents/hami-mock-monitor/internal/hami"
)

type Completer struct {
	client kubernetes.Interface
	pods   listerscorev1.PodLister
	node   string
}

func New(client kubernetes.Interface, pods listerscorev1.PodLister, node string) *Completer {
	return &Completer{client: client, pods: pods, node: node}
}

// Reconcile completes the bind of the pod the node lock names, if that pod is
// bound to this node, still Pending and allocating: it patches the pod to
// success, then releases the lock. A pod already at success only gets its
// lock released. Reconcile is idempotent and touches nothing else; without a
// Pending candidate in the cache it makes no API call.
func (c *Completer) Reconcile(ctx context.Context) error {
	pods, err := c.pods.List(labels.SelectorFromSet(labels.Set{hami.AssignedNodeLabel: c.node}))
	if err != nil {
		return err
	}
	candidates := map[types.NamespacedName]*corev1.Pod{}
	for _, p := range pods {
		phase := p.Annotations[hami.BindPhaseAnnotation]
		if p.Spec.NodeName == c.node && p.Status.Phase == corev1.PodPending &&
			(phase == hami.BindPhaseAllocating || phase == hami.BindPhaseSuccess) {
			candidates[types.NamespacedName{Namespace: p.Namespace, Name: p.Name}] = p
		}
	}
	if len(candidates) == 0 {
		return nil
	}

	node, err := c.client.CoreV1().Nodes().Get(ctx, c.node, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get node %s: %w", c.node, err)
	}
	ns, name, ok := hami.ParseNodeLock(node.Annotations[hami.NodeLockAnnotation])
	if !ok {
		return nil
	}
	pod := candidates[types.NamespacedName{Namespace: ns, Name: name}]
	if pod == nil {
		return nil
	}

	if pod.Annotations[hami.BindPhaseAnnotation] == hami.BindPhaseAllocating {
		// The uid makes the patch fail rather than land on a successor pod
		// that reused the name.
		patch, err := json.Marshal(map[string]any{"metadata": map[string]any{
			"uid":         pod.UID,
			"annotations": map[string]string{hami.BindPhaseAnnotation: hami.BindPhaseSuccess},
		}})
		if err != nil {
			return err
		}
		if _, err := c.client.CoreV1().Pods(ns).Patch(ctx, name, types.MergePatchType, patch, metav1.PatchOptions{}); err != nil {
			return fmt.Errorf("complete bind of %s/%s: %w", ns, name, err)
		}
		slog.Info("completed bind", "pod", ns+"/"+name)
	}

	// The resourceVersion makes the release conditional on the lock read above.
	patch, err := json.Marshal(map[string]any{"metadata": map[string]any{
		"resourceVersion": node.ResourceVersion,
		"annotations":     map[string]any{hami.NodeLockAnnotation: nil},
	}})
	if err != nil {
		return err
	}
	if _, err := c.client.CoreV1().Nodes().Patch(ctx, c.node, types.MergePatchType, patch, metav1.PatchOptions{}); err != nil {
		return fmt.Errorf("release node lock of %s: %w", c.node, err)
	}
	slog.Info("released node lock", "node", c.node, "pod", ns+"/"+name)
	return nil
}
