# Distributed Skopeo Registry Clone

This directory defines the Kubernetes runtime shape for a distributed,
blob-deduplicated registry clone based on this Skopeo fork.

It is intentionally small:

- Redis StatefulSet: queues, status, and manifest DAG counters.
- `metadata-worker` Deployment: consumes tag input, reads manifests, emits unique blob tasks and metadata dependencies.
- `blob-worker` Deployment: high-concurrency `copy-blob` data plane, optimized for network throughput.
- `publish-worker` Deployment: resolves dependency in-degree and publishes ready manifests/tags.
- `submit-tags` Job: submits source/target image references into Redis.

## Current Interface Contract

The deployment expects the Skopeo image to expose these commands:

```bash
skopeo registry-clone submit-tags --file /input/tags.txt
skopeo registry-clone metadata-worker
skopeo registry-clone blob-worker
skopeo registry-clone publish-worker
```

These commands are the target interface for the distributed version. The
existing `skopeo copy-blob` command is the blob transfer primitive that the
`blob-worker` should call or embed.

## Queue Model

Redis streams:

```text
stream:tags
stream:blobs
stream:blob-events
stream:publish
stream:manifest-events
```

Redis keys:

```text
blob:{digest}
manifest:{digest}
tag:{repo}:{tag}
dependents:blob:{digest}
dependents:manifest:{digest}
seen:blob:{digest}
seen:manifest:{digest}
```

## One-Command Deploy

Edit `secret.example.yaml` first, or replace it with a real secret generated
outside the repo:

```bash
kubectl -n registry-clone create secret generic registry-clone-auth \
  --from-file=source-auth.json=./source-auth.json \
  --from-file=target-auth.json=./target-auth.json
```

Then apply:

```bash
kubectl apply -k deploy/registry-clone
```

Submit tags by editing `submit-tags-configmap.yaml` and re-running the Job:

```bash
kubectl delete job -n registry-clone registry-clone-submit-tags --ignore-not-found
kubectl apply -k deploy/registry-clone
```

## Scale Blob Workers

`blob-worker` is the data plane. Scale it to use more nodes and aggregate
network throughput:

```bash
kubectl scale deploy/registry-clone-blob-worker -n registry-clone --replicas=50
```

Tune per-pod concurrency through `BLOB_LOCAL_CONCURRENCY` in
`configmap.yaml`.

## Metrics Contract

Each worker exposes Prometheus metrics on:

```text
0.0.0.0:9090/metrics
```

The Kubernetes Services in `metrics-services.yaml` include standard scrape
annotations:

```text
prometheus.io/scrape=true
prometheus.io/port=9090
prometheus.io/path=/metrics
```

### Required Metrics

Queue and scheduling:

```text
registry_clone_stream_pending{stream,group}
registry_clone_stream_lag{stream,group}
registry_clone_tasks_claimed_total{role,stream}
registry_clone_tasks_retried_total{role,stream,reason}
registry_clone_task_duration_seconds_bucket{role,task_type,status}
registry_clone_inflight_tasks{role,task_type}
```

Metadata discovery:

```text
registry_clone_tags_discovered_total
registry_clone_manifests_discovered_total{kind}
registry_clone_blob_refs_total{kind}
registry_clone_unique_blobs_total{kind}
registry_clone_unique_blob_bytes{kind}
registry_clone_manifest_parse_errors_total{reason}
```

Blob copy data plane:

```text
registry_clone_blob_copy_total{status,kind}
registry_clone_blob_copy_bytes_total{status,kind}
registry_clone_blob_copy_duration_seconds_bucket{status,kind}
registry_clone_blob_copy_inflight
registry_clone_blob_copy_errors_total{reason}
registry_clone_registry_http_errors_total{registry,operation,status_code}
```

Deduplication and ensure semantics:

```text
registry_clone_blob_refs_deduped_total{kind}
registry_clone_blob_bytes_skipped_total{reason}
registry_clone_blob_existing_total{kind}
```

Manifest and tag publishing:

```text
registry_clone_publish_total{object,status}
registry_clone_publish_duration_seconds_bucket{object,status}
registry_clone_manifest_indegree{kind}
registry_clone_ready_publish_queue_depth{object}
registry_clone_publish_errors_total{object,reason}
```

### Dashboard Signals

The first dashboard should answer five questions:

```text
Is stream:blobs deep enough to keep blob workers busy?
Are source/target bytes/sec close to the expected network limit?
How much traffic is saved by already_exists / dedupe?
Are 429/503/timeouts causing backoff?
Are manifests/tags blocked by nonzero in_degree?
```

Suggested headline panels:

```text
Blob copy throughput bytes/sec
Blob copy status rate: copied vs already_exists vs failed
Unique blob bytes vs logical blob reference bytes
stream:blobs lag and pending entries
publish queue depth and manifest in_degree p95
HTTP errors by registry and status code
```

## Worker Responsibilities

### metadata-worker

Consumes `stream:tags`, reads source manifests and indexes, then writes:

- unique blob hashes and `stream:blobs`
- manifest hashes and raw manifests
- dependency reverse indexes
- tag objects

### blob-worker

Consumes `stream:blobs`, runs ensure-copy semantics:

```text
target already has digest -> status already_exists
otherwise stream source blob -> target blob -> status copied
```

The current primitive is:

```bash
skopeo copy-blob SOURCE-IMAGE DESTINATION-IMAGE DIGEST
```

### publish-worker

Consumes blob and manifest events, decrements dependency in-degree, and
publishes ready manifests, indexes, and tags to the target registry.

## Notes

- No Docker daemon or host image store is required.
- Pods do not need privileged mode or `/var/run/docker.sock`.
- Redis persistence is enabled with AOF and a 20Gi PVC by default.
- For production, prefer an external managed Redis if the clone spans many
  nodes or long-running TB-scale migrations.
