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
completes HAMi's bind (see [Bind completion](#bind-completion)).

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
| `hami_container_device_utilization_ratio` | same | SM utilization, 0-100 |
| `hami_host_gpu_utilization_ratio` | `node, device_index, device_uuid, device_type` | sum over the device's containers, capped at 100 |
| `hami_host_gpu_memory_used_bytes` | same | sum over the device's containers, capped at the device memory |

`vdevice_index` is the device's position within the container's allocation.
`device_type` is the registered model, prefixed `NVIDIA-` when it lacks the
prefix, as HAMi labels it. `device_index` is the registered `index`, or the
list position when the registration leaves indices out.

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
| `max-num-seqs` | vllm | running requests that mean 100% utilization | `--vllm-max-num-seqs` (`5`) |
| `seed` | seed | seed of the curves | `--seed` (`0`) |
| `utilization` | static | SM utilization, 0-100 | `--static-utilization` (`50`) |
| `memory-fraction` | static | share of the memory limit in use, 0-1 | `--static-memory-fraction` (`0.5`) |

- **vllm** scrapes `http://<podIP>:<port><path>` every `--scrape-interval`
  (15s). Without the port annotation it uses the first container port named
  `metrics`, else one named `http`, else `--vllm-port`.
  `vllm:num_requests_running / max-num-seqs` becomes the utilization (capped at
  100) and `vllm:kv_cache_usage_perc` (0-1; `vllm:gpu_cache_usage_perc` on vLLM
  before 0.10) the memory fraction. Series of several engines are summed
  (requests) and averaged (cache). The values apply to every device of every
  container in the pod. A failed scrape keeps the last value for
  `--stale-after` (1m); after that the series are dropped until a scrape
  succeeds again. Works with vLLM and with
  [llm-d-inference-sim](https://github.com/llm-d/llm-d-inference-sim).
- **seed** follows a sine curve between `--seed-min` and `--seed-max` percent
  (10, 90) with period `--seed-period` (10m), for utilization and memory
  fraction alike. Each pod, container and device gets its own phase,
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
step instead: when the node lock names a pod that is bound to this node,
Pending and `allocating`, it patches the pod to `success` (conditional on its
UID) and releases the lock (conditional on the node's resourceVersion). It runs
on every pod event and every `--complete-binds-interval` (2s), and is
idempotent. It is a workaround until the mock device plugin does this itself;
[upstream/mock-device-plugin](upstream/mock-device-plugin) holds the prepared
patch. It needs `patch` on pods and nodes, which the chart grants only when it
is on.

## Install

```sh
helm install hami-mock-monitor charts/hami-mock-monitor -n kube-system
```

The chart runs a DaemonSet (with `NODE_NAME` from the downward API), its
ServiceAccount and ClusterRole (pods `list`/`watch`, nodes `get`; plus pods and
nodes `patch` with `completeBinds`), a Service on 9394 and, with
`serviceMonitor.enabled`, a ServiceMonitor. No `nodeSelector` by default; set
one to run only on the mocked GPU nodes. Scrape the endpoints (one per node),
not the Service's virtual IP. Further flags go in `extraArgs`; see
`hami-mock-monitor --help`.

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
  --set completeBinds=true
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
        - '--fake-metrics={"running-requests":"oscillate:0:5:2m","kv-cache-usage":"oscillate:0.1:0.8:2m"}'
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
