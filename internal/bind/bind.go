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
	"errors"
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

// Reconcile completes every open bind on this node: each pod HAMi assigned and
// bound here that has not finished and is still allocating, Pending or already
// Running, is patched to success. The node lock is released when it names one
// of them or any other pod of this node at success: the device plugin and this
// completer both set success before they release the lock, so such a lock is
// stale (a release that failed or a run that died between the two patches).
// Reconcile is idempotent; without such a pod in the cache it makes no API call.
func (c *Completer) Reconcile(ctx context.Context) error {
	pods, err := c.pods.List(labels.SelectorFromSet(labels.Set{hami.AssignedNodeLabel: c.node}))
	if err != nil {
		return err
	}
	var open []*corev1.Pod
	// done holds the pods whose bind is complete; the lock may still name one.
	done := map[types.NamespacedName]bool{}
	for _, p := range pods {
		if p.Spec.NodeName != c.node {
			continue
		}
		switch p.Annotations[hami.BindPhaseAnnotation] {
		case hami.BindPhaseAllocating:
			if p.Status.Phase != corev1.PodSucceeded && p.Status.Phase != corev1.PodFailed {
				open = append(open, p)
			}
		case hami.BindPhaseSuccess:
			done[types.NamespacedName{Namespace: p.Namespace, Name: p.Name}] = true
		}
	}
	if len(open) == 0 && len(done) == 0 {
		return nil
	}

	var errs []error
	for _, p := range open {
		// The uid makes the patch fail rather than land on a successor pod
		// that reused the name.
		patch, err := json.Marshal(map[string]any{"metadata": map[string]any{
			"uid":         p.UID,
			"annotations": map[string]string{hami.BindPhaseAnnotation: hami.BindPhaseSuccess},
		}})
		if err != nil {
			return err
		}
		if _, err := c.client.CoreV1().Pods(p.Namespace).Patch(ctx, p.Name, types.MergePatchType, patch, metav1.PatchOptions{}); err != nil {
			errs = append(errs, fmt.Errorf("complete bind of %s/%s: %w", p.Namespace, p.Name, err))
			continue
		}
		done[types.NamespacedName{Namespace: p.Namespace, Name: p.Name}] = true
		slog.Info("completed bind", "pod", p.Namespace+"/"+p.Name)
	}
	return errors.Join(append(errs, c.releaseLock(ctx, done))...)
}

// releaseLock removes the node lock if it names a pod in done.
func (c *Completer) releaseLock(ctx context.Context, done map[types.NamespacedName]bool) error {
	node, err := c.client.CoreV1().Nodes().Get(ctx, c.node, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get node %s: %w", c.node, err)
	}
	ns, name, ok := hami.ParseNodeLock(node.Annotations[hami.NodeLockAnnotation])
	if !ok || !done[types.NamespacedName{Namespace: ns, Name: name}] {
		return nil
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
