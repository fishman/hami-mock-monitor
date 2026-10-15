# feat(mock): complete the HAMi bind in Allocate

Patch: `0001-feat-mock-complete-the-HAMi-bind-in-Allocate.patch` (against
`Project-HAMi/mock-device-plugin` main at `8be1424`; `git am` applies it).

## Problem

HAMi's scheduler binds a pod in two steps. `Bind` takes the node lock
(`hami.io/mutex.lock`, value `RFC3339,namespace,name`) and marks the pod
`hami.io/bind-phase=allocating`. The device plugin finishes it at the end of
`Allocate`: HAMi's NVIDIA plugin calls `PodAllocationTrySuccess`, which patches
`hami.io/bind-phase=success` and releases the node lock.

The mock plugin's `Allocate` only returns empty container responses. With it:

- every mocked pod stays `allocating` forever, unlike on a node with a real
  device plugin, so tooling that keys on `success` (monitors, metering built on
  HAMi) never sees the pod as allocated;
- the node lock is held until it expires (`HAMI_NODELOCK_EXPIRE`, 5 minutes by
  default), so the next pod on the node waits that long before it can be bound.

## Change

- `internal/pkg/mock/bind.go`: `completeBind` resolves the pod the way HAMi's
  `util.GetPendingPod` does (the pod the node lock names; without a lock owner,
  a Pending pod on this node with `hami.io/bind-time`, `bind-phase=allocating`
  and `hami.io/vgpu-node` = this node), patches `hami.io/bind-phase=success`
  with the pod UID as a precondition, and releases the lock with the node's
  resourceVersion as a precondition.
- `MockPlugin.Allocate` calls it after building the response. A failure is
  logged and never fails `Allocate`: the container still starts, and HAMi's lock
  still expires on its own.
- kubelet calls `Allocate` once per mocked resource a container requests
  (`gpumem`, `gpucores`, `gpumem-percentage` are separate plugins), so the
  completion is idempotent: once the pod is marked and the lock released, later
  calls find nothing to do.
- `k8s-mock-rbac.yaml`: the plugin's ClusterRole gains pods `get`, `list`,
  `patch`. Without it the pod lookup or patch is rejected with 403 and only the
  log line remains.

No new direct dependencies; it uses the existing `util/client` and stays on
client-go 0.28 (`go.mod`/`go.sum` only gain the fake clientset's indirect
requirements for the tests).

## Companion change in Project-HAMi/HAMi

HAMi's chart deploys the mock plugin with its own ClusterRole
(`charts/hami/templates/scheduler/clusterrole.yaml`, the mock device plugin
rules), which grants nodes only. It needs the same rule, in a separate PR:

```yaml
  - apiGroups: [""]
    resources: ["pods"]
    verbs: ["get", "list", "patch"]
```

## Limitations

- Completion runs only when kubelet calls the mock's `Allocate`, that is for
  containers that request a mocked resource (`nvidia.com/gpumem`,
  `nvidia.com/gpucores`, `nvidia.com/gpumem-percentage`). A pod that asks for
  `nvidia.com/gpu` alone reaches no device plugin, as that count resource is
  patched onto the node status by hand in a mock setup.
- Like HAMi's own plugins, the owner-less fallback picks the first matching
  Pending pod; it does not match the allocation request to a specific pod.

## Testing

- `go test ./...` (new `internal/pkg/mock/bind_test.go`, fake clientset): only
  the pod the lock names is completed and the lock released; a second call
  patches nothing; without a lock the Pending allocating pod is completed and a
  Running one is not; a lock naming a missing pod fails without patching
  anything.
- Not yet exercised against a live cluster with this build of the plugin.
