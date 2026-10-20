# hami-mock-monitor

A stand-in for [HAMi](https://github.com/Project-HAMi/HAMi)'s vGPU monitor on
clusters that run HAMi's [mock device plugin](https://github.com/Project-HAMi/mock-device-plugin).
It serves the vGPUmonitor metric contract (`hami_*` on `:9394/metrics`) for the
pods HAMi allocated on its node, with usage taken from a load source instead
of real GPUs: a vLLM-compatible `/metrics` endpoint, a seeded curve, or fixed
values.

## Why

The mock device plugin lets HAMi's scheduler hand out fake GPUs without
hardware, which is enough to test scheduling. It is not enough for anything
built on HAMi's usage metrics (dashboards, HAMi-WebUI, recording rules,
metering): those come from HAMi's vGPUmonitor, which reads HAMi-core's shared
memory regions and NVML, and neither exists without a GPU. On a mock cluster
the monitor has nothing to read, so the usage series never appear.

hami-mock-monitor fills that gap. It reads only what HAMi already writes to the
Kubernetes API (the pods' allocation annotations and the node's device
registration) and fills in usage from a load source, so the series have the
same names, labels and units as on a real node.

It also works around a gap in the mock device plugin: its `Allocate` never
completes HAMi's bind (see [Bind completion](#bind-completion)). And it brings
a [mock nvidia-smi](#mock-nvidia-smi), for the node and inside the containers.

## HAMi compatibility

Built against the HAMi v2.10 conventions:

| Convention | Used for |
| :-- | :-- |
| pod label `hami.io/vgpu-node` | pods HAMi assigned to the node |
| pod annotation `hami.io/vgpu-devices-allocated` (`uuid,type,mem_mib,cores:` per device, `;` per container, init containers first) | per-container devices and memory limits |
| pod annotation `hami.io/bind-phase` | which pods count |
| node annotation `hami.io/node-nvidia-register` (JSON list) | host devices: index, model, memory |
| node annotation `hami.io/mutex.lock` (`RFC3339,namespace,name`) | bind completion |

Older HAMi releases that register devices in the colon-separated form are not
supported. NVIDIA devices only. With HAMi 2.10 the mock device plugin needs a
build newer than 1.0.1, which cannot parse the map-form `vnpus` section of
2.10's device ConfigMap.

## Metrics

Every series carries `zone="vGPU"`, as HAMi's monitor does.

| Metric | Labels | Value |
| :-- | :-- | :-- |
| `hami_vgpu_memory_used_bytes` | `namespace, pod, container, vdevice_index, device_uuid` | memory fraction x limit |
| `hami_vgpu_memory_limit_bytes` | same | allocated memory (`mem_mib` x 2^20) |
| `hami_container_device_memory_bytes` | same | same as `hami_vgpu_memory_used_bytes` |
| `hami_container_device_utilization_ratio` | same | SM utilization in percent of the whole device: load x allocated cores (cores 0 = the whole device, 100) |
| `hami_host_gpu_utilization_ratio` | `node, device_index, device_uuid, device_type` | sum over the device's containers, capped at 100 |
| `hami_host_gpu_memory_used_bytes` | same | sum over the device's containers, capped at the device memory |
| `hami_host_gpu_temperature_celsius` | same | derived from the device's utilization: 30 idle to 80 at 100, whole degrees |
| `hami_host_gpu_power_usage_watts` | same | derived from the device's utilization: 60 idle to the 400 W cap at 100 |

Every load source yields a share of the container's allocation, 0-1. The
utilization multiplies it by the allocated cores from
`hami.io/vgpu-devices-allocated`, so a container at full load on 30 cores
reports 30, as HAMi's monitor reports the container's share of the device and
consumers sum it. `vdevice_index` is the device's position within the
container's allocation.
`device_type` is the registered model, prefixed `NVIDIA-` when it lacks the
prefix, as HAMi labels it. `device_index` is the registered `index`, or the
list position when the registration leaves indices out.
Temperature and power use the derivation the [mock nvidia-smi](#mock-nvidia-smi)
shows, so dashboards and `nvidia-smi` agree.

With `--legacy-metrics` the legacy names are emitted as well:
`HostGPUMemoryUsage`, `HostCoreUtilization`,
`vGPU_device_memory_usage_in_bytes`, `vGPU_device_memory_limit_in_bytes`,
`Device_memory_desc_of_container`, `Device_utilization_desc_of_container`.

Only pods with `hami.io/bind-phase=success` count, bound to this node and not
finished. `--count-allocating` also counts pods still at `allocating`, which is
where an unpatched mock device plugin leaves them.

An unknown value is never reported as zero: when a container's load is unknown
(a vLLM endpoint that stopped answering, see below), its series are dropped,
and so are the host series of each device it uses.

## Load sources

Each pod picks its source with an annotation; `--load-source` sets the default
(`seed`). All annotations are optional and override the matching flag.

| Annotation (`hami-mock-monitor.io/...`) | Source | Meaning | Flag (default) |
| :-- | :-- | :-- | :-- |
| `load-source` | all | `vllm`, `seed` or `static` | `--load-source` (`seed`) |
| `metrics-port` | vllm | port of the metrics endpoint | `--vllm-port` (`8000`) |
| `metrics-path` | vllm | path of the metrics endpoint | `--vllm-path` (`/metrics`) |
| `max-num-seqs` | vllm | running requests that mean full load | `--vllm-max-num-seqs` (`5`) |
| `seed` | seed | seed of the curves | `--seed` (`0`) |
| `utilization` | static | percent of the allocated cores in use, 0-100 | `--static-utilization` (`50`) |
| `memory-fraction` | static, vllm | share of the memory limit in use, 0-1 | `--static-memory-fraction` (`0.5`), `--vllm-memory-fraction` (`0.9`) |

- **vllm** scrapes `http://<podIP>:<port><path>` every `--scrape-interval`
  (15s). Without the port annotation it uses the first container port named
  `metrics`, else one named `http`, else `--vllm-port`.
  `vllm:num_requests_running / max-num-seqs` (capped at 1, summed over
  engines) becomes the load. Memory in use is `memory-fraction` x the memory
  limit, constant while the endpoint answers: vLLM pre-allocates
  `--gpu-memory-utilization` (default 0.9) of the device memory at startup and
  keeps its KV cache inside that pool, so a real vLLM pod uses about that much
  memory whether idle or busy, and `vllm:kv_cache_usage_perc` only says how
  full the pool is. Set the annotation to the pod's
  `--gpu-memory-utilization` when it differs. The values apply to every device
  of every container in the pod. A failed scrape keeps the last value for
  `--stale-after` (1m, at least `--scrape-interval`); after that the series
  are dropped, memory included, until a scrape succeeds again. Works with vLLM and with
  [llm-d-inference-sim](https://github.com/llm-d/llm-d-inference-sim).
- **seed** follows a sine curve between `--seed-min` and `--seed-max` percent
  (10, 90) of the allocation with period `--seed-period` (10m), for load and
  memory fraction alike. Each pod, container and device gets its own phase,
  `sha256(seed, namespace/pod/container/device)`, so the same seed always
  produces the same values at the same time.
- **static** reports fixed values.

A pod with an invalid annotation is logged and left out.

## Bind completion

HAMi's scheduler binds a pod in two steps: `Bind` takes the node lock
`hami.io/mutex.lock` and marks the pod `hami.io/bind-phase=allocating`; the
device plugin's `Allocate` then marks it `success` and releases the lock. The
mock device plugin's `Allocate` does neither, so mocked pods stay `allocating`
and the next pod on the node waits for the lock to expire (5 minutes by
default).

`--complete-binds` (chart: `completeBinds: true`, off by default) does that
step instead. Every pod HAMi assigned and bound to this node that has not
finished and is still `allocating` is patched to `success` (conditional on its
UID), whether still Pending or already Running: on a mock node the container
often starts before the monitor sees the pod. The node lock is released only
when it names one of those pods or another pod of this node already at
`success`, since such a lock is stale (conditional on the node's
resourceVersion). Pods and the node come from informer caches (the node through
a `metadata.name` field selector), so a run only writes what needs changing.
It runs on every pod and node event, so a lock is released as soon as it is
written; `--complete-binds-interval` (2s) only covers missed events. It is
idempotent. It is a workaround until the mock device plugin does this itself;
[upstream/mock-device-plugin](upstream/mock-device-plugin) holds the prepared
patch. It needs `patch` on pods and nodes, which the chart grants only when it
is on.

## Mock nvidia-smi

`hami-mock-monitor smi` prints the node's mock GPUs the way `nvidia-smi`
(driver 580) does, from the monitor's in-memory state: the snapshot the
metrics are built from, served as JSON on `GET /smi` on the metrics port.

```sh
kubectl -n kube-system exec ds/hami-mock-monitor -- hami-mock-monitor smi
hami-mock-monitor smi --node gpu-node-1   # from outside, through the API server
```

```
Fri Oct 16 09:30:00 2026
+-----------------------------------------------------------------------------------------+
| NVIDIA-SMI 580.105.08 MOCK        Driver Version: 580.105.08     CUDA Version: 13.0     |
+-----------------------------------------+------------------------+----------------------+
| GPU  Name                 Persistence-M | Bus-Id          Disp.A | Volatile Uncorr. ECC |
| Fan  Temp   Perf          Pwr:Usage/Cap |           Memory-Usage | GPU-Util  Compute M. |
|                                         |                        |               MIG M. |
|=========================================+========================+======================|
|   0  NVIDIA-A100-SXM4-40GB          On  |   00000000:01:00.0 Off |                  N/A |
| N/A   62C    P0            280W /  400W |   29695MiB /  40960MiB |     65%      Default |
|                                         |                        |                  N/A |
+-----------------------------------------+------------------------+----------------------+
|   1  NVIDIA-A100-SXM4-40GB          On  |   00000000:02:00.0 Off |                  N/A |
| N/A   30C    P8             60W /  400W |       0MiB /  40960MiB |      0%      Default |
|                                         |                        |                  N/A |
+-----------------------------------------+------------------------+----------------------+

+-----------------------------------------------------------------------------------------+
| Processes:                                                                              |
|  GPU   GI   CI              PID   Type   Process name                        GPU Memory |
|        ID   ID                                                               Usage      |
|=========================================================================================|
|    0   N/A  N/A         2268734      C   team-a/llm-0/vllm                     12287MiB | (13653MiB alloc)
|    0   N/A  N/A         1922006      C   team-b/train-7f9c/eval                 1024MiB | (4096MiB alloc)
|    0   N/A  N/A         3317123      C   team-b/train-7f9c/trainer             16384MiB | (20480MiB alloc)
+-----------------------------------------------------------------------------------------+
```

The header says `MOCK` and `uuid` is the UUID from HAMi's device
registration, so the output is not mistaken for real hardware. Per GPU: the
registered index and type, a Bus-Id derived from the index
(`00000000:<index+1 hex>:00.0`), memory used as the sum of the containers'
usage, GPU-Util from the load sources; temperature (30 C idle to 80 C) and
power (60 W idle to the 400 W cap) follow utilization, perf is `P0` when busy
and `P8` when idle. One process row per device of each allocated container:
a stable fake PID (a hash of pod UID and container), type `C`,
`<namespace>/<pod>/<container>`, its usage, and right of the row its
allocation, so used and allocated are both visible. Values whose load is
unknown show `N/A`.

Both views also answer `--query-gpu=` with `index, name, uuid, pci.bus_id,
memory.total, memory.used, memory.free, utilization.gpu, temperature.gpu,
power.draw, power.limit` and `--query-compute-apps=pid,process_name,used_memory`,
with `--format=csv[,noheader][,nounits]`. Unknown fields fail as in
nvidia-smi (`Field "x" is not a valid field to query.`, exit 2); without a
device the answer is `No devices were found` (exit 6), without a monitor
exit 9.

`--monitor-url` (default `http://localhost:9394`, the monitor in the same pod)
names the monitor to ask. `--node` asks the monitor pod on that node
(`app.kubernetes.io/name=hami-mock-monitor`) through the API server's pod
proxy with the kubeconfig, which needs `list` on pods and `get` on
`pods/proxy`.

### Inside containers

With `smi.inject.enabled=true` the chart registers a mutating webhook the
monitor serves (`failurePolicy: Ignore`) for new pods in namespaces matching
`smi.inject.namespaceSelector` (default: all but `kube-system`) that HAMi
allocates to. HAMi writes `hami.io/vgpu-devices-allocated` only when it
schedules a pod, after admission, so a pod qualifies by a limit on one of
`smi.inject.resourceNames` (HAMi's NVIDIA resources) or by carrying the
annotation already. Other pods are left untouched. The webhook adds:

- an init container from the monitor image that copies the binary into an
  emptyDir (`hami-mock-monitor install-smi`; the image has no shell),
- the binary as the single file `/usr/local/bin/nvidia-smi` (subPath) in
  every container,
- `HAMI_MOCK_SMI_URL` (the chart's Service, `internalTrafficPolicy: Local`,
  so a container reaches the monitor of its own node), `POD_NAMESPACE` and
  `POD_NAME` (downward API) and `HAMI_MOCK_SMI_CONTAINER`. Variables the
  container sets itself win.

Called as `nvidia-smi`, the binary asks for `GET /smi?namespace=&pod=&container=`
and shows the container's slice as HAMi's libvgpu does: one GPU per allocated
vGPU numbered from 0, its memory limit as total, its own usage and
utilization share, itself as the only process.

```sh
kubectl exec sim -- nvidia-smi
kubectl exec sim -- nvidia-smi --query-gpu=memory.used,memory.total --format=csv
```

The webhook certificate comes from a CA the chart generates on install and
keeps across upgrades (`lookup`, so `helm template` renders a new one), or
from cert-manager with `smi.inject.certManager.enabled=true` (a self-signed
Issuer, CA injection into the webhook configuration). The monitor reads it on
every handshake, so renewals need no restart. The init container pulls the
monitor image with the pod's own pull secrets.

## Install

```sh
helm install hami-mock-monitor charts/hami-mock-monitor -n kube-system
```

The chart runs a DaemonSet (with `NODE_NAME` from the downward API), its
ServiceAccount and ClusterRole (pods `list`/`watch`, nodes `get`/`list`/`watch`;
plus pods and nodes `patch` with `completeBinds`), a Service on 9394 with
`internalTrafficPolicy: Local` and, with `serviceMonitor.enabled`, a
ServiceMonitor; with `smi.inject.enabled` also the webhook, its Service and
certificate. No `nodeSelector` by default; set one to run only on the mocked
GPU nodes. Scrape the endpoints (one per node), not the Service's virtual IP.
Further flags go in `extraArgs`; see `hami-mock-monitor --help`.

## Quickstart: kind, HAMi, mock device plugin, llm-d-inference-sim

```sh
kind create cluster --name hami-mock
NODE=hami-mock-control-plane
kubectl label node $NODE gpu=on

helm repo add hami-charts https://project-hami.github.io/HAMi/
helm install hami hami-charts/hami -n kube-system --version 2.10.0 \
  --set devicePlugin.enabled=false \
  --set mockDevicePlugin.enabled=true \
  --set mockDevicePlugin.image.tag=latest

# Two fake A100s: the count resource (the mock plugin's health gate) and the
# device registration the real device plugin would write.
kubectl patch node $NODE --subresource=status --type=json \
  -p '[{"op":"add","path":"/status/capacity/nvidia.com~1gpu","value":"20"}]'
kubectl annotate node $NODE --overwrite 'hami.io/node-nvidia-register=[
  {"id":"GPU-mock-0","index":0,"count":10,"devmem":40960,"devcore":100,"type":"NVIDIA-A100-SXM4-40GB","health":true,"numa":0,"mode":"hami-core"},
  {"id":"GPU-mock-1","index":1,"count":10,"devmem":40960,"devcore":100,"type":"NVIDIA-A100-SXM4-40GB","health":true,"numa":0,"mode":"hami-core"}]'

make image IMG=hami-mock-monitor:dev
kind load docker-image hami-mock-monitor:dev --name hami-mock
helm install hami-mock-monitor charts/hami-mock-monitor -n kube-system \
  --set image.repository=hami-mock-monitor --set image.tag=dev \
  --set completeBinds=true --set smi.inject.enabled=true
kubectl -n kube-system rollout status ds/hami-mock-monitor
```

An inference simulator on a third of a card, its load oscillating over two
minutes:

```sh
kubectl apply -f - <<'EOF'
apiVersion: v1
kind: Pod
metadata:
  name: sim
  annotations:
    hami-mock-monitor.io/load-source: vllm
spec:
  containers:
    - name: sim
      image: ghcr.io/llm-d/llm-d-inference-sim:v0.12.0
      args:
        - --model=fake-model
        - --port=8000
        - --max-num-seqs=5
        - '--fake-metrics={"running-requests":"oscillate:0:5:2m"}'
      ports:
        - name: http
          containerPort: 8000
      resources:
        limits:
          nvidia.com/gpu: 1
          nvidia.com/gpumem: 13653
          nvidia.com/gpucores: 33
EOF

kubectl get pod sim -o jsonpath='{.metadata.annotations.hami\.io/bind-phase}{"\n"}'  # success
kubectl -n kube-system port-forward ds/hami-mock-monitor 9394 &
curl -s localhost:9394/metrics | grep '^hami_'
kubectl exec sim -- nvidia-smi
```

## Upstream patch

[upstream/mock-device-plugin](upstream/mock-device-plugin) contains a
`git format-patch` patch for `Project-HAMi/mock-device-plugin` (client-go 0.28)
that completes the bind in `MockPlugin.Allocate` and grants the plugin pods
`get`/`list`/`patch`, with its PR description. Until it is merged, use
`--complete-binds` or `--count-allocating`. The patched plugin only sees pods
that request a mocked resource (`nvidia.com/gpumem`, `nvidia.com/gpucores`,
`nvidia.com/gpumem-percentage`); `--complete-binds` covers every pod.

## Development

```sh
make build       # bin/hami-mock-monitor
make test        # go test ./...
make lint        # gofmt + go vet
make image       # docker build (IMG=hami-mock-monitor:dev)
make chart-test  # helm lint + helm template (HELM=helm)
```

Go 1.27, client-go 0.37, the same versions as HAMi 2.10.

## License

Apache-2.0, see [LICENSE](LICENSE).
