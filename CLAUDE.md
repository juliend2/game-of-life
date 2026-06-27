# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this repo is

Conway's Game of Life implemented on Kubernetes: each living cell is a Kubernetes `Job` running a Go agent. Cells coordinate through a shared Redis registry. A single long-lived "perceiver" pod watches the grid and re-seeds when extinction is detected.

## Architecture

Three components in three top-level directories:

- `agent/` — Go binary. One pod per living cell. Reads `CELL_X`/`CELL_Y` from env, registers `cell:X:Y` in Redis, ticks every `TICK_INTERVAL` seconds, applies GoL rules, and exits when it dies. Spawns new agent Jobs via the Kubernetes API for cells that should be born. The same binary doubles as the seeder when `SEED_MODE=true`.
- `perceiver/` — Go binary, single `Deployment`. Polls Redis for the alive set, serves an HTTP dashboard on `:8080` (`GET /grid`, `GET /status`, `GET /healthz`), and re-seeds the world (picks a random pattern from `SEED_PATTERNS`) after `EXTINCTION_GRACE_TICKS` consecutive empty polls.
- `internal/spawn/` — Shared Go package that builds and submits the cell Job. Imported by both binaries.
- `k8s/` — All manifests: namespace, RBAC (separate ServiceAccounts for agent vs perceiver), ConfigMap, Redis (PVC-backed), perceiver Deployment + NodePort 30080, seed Job.

Cross-cutting things worth knowing before editing:

- **Cells are Kubernetes Jobs, not Pods.** Jobs don't get restarted by the kubelet when the container exits — a dying cell stays dead. Job names are deterministic: `gol-cell-<x>-<y>` (see `spawn.JobName`).
- **Job construction lives in `internal/spawn`.** Both binaries call `spawn.Cell(ctx, kube, cfg.spawnConfig(), x, y)`. Change Job shape (env, resources, labels, service account) there — it's the single source of truth.
- **Race-free spawning.** Two surviving neighbors can independently decide the same dead cell should be born. The agent acquires a Redis `SETNX` lock on `spawning:X:Y` (30s TTL) via `trySpawn` before calling `spawn.Cell`; the perceiver and the seeder skip the lock and rely on the deterministic Job name + `IsAlreadyExists`, which `spawn.Cell` treats as a success.
- **Cleanup on exit is load-bearing.** Each agent does `defer deregister(...)` on its own `cell:X:Y` key. This runs on graceful exit, SIGTERM from kubelet, panic — anything. Don't add early returns in `main` that bypass this `defer`.
- **Grid edges don't wrap.** Cells at `x=0` or `y=gridHeight-1` simply have fewer neighbors.
- **`tick_interval` must give new pods time to come up.** The ConfigMap default is 10s. Going lower than that risks evaluating a neighbor before its agent has registered itself in Redis.

## Common commands

There is a single top-level Go module (`go.mod` at the repo root). Builds and image work go through the root `Makefile`.

```bash
# Build both binaries
make build              # or: make build-agent / make build-perceiver

# Build & push both images (uses REGISTRY=localhost:5000 by default)
make docker push        # or: make docker-agent push-agent / docker-perceiver push-perceiver

# Override registry
make docker REGISTRY=ghcr.io/yourname

# Module hygiene
make tidy
```

Dockerfiles expect the build context to be the **repo root** (not the per-binary directory) because they need to see `internal/`. The Makefile handles this — run docker builds via `make`, not by hand.

Local dev runs against a k3s cluster with a Docker registry at `localhost:5000`. Full setup in `k8s/LOCAL_DEV_SETUP.md`. The deploy/observe loop:

```bash
# Deploy (apply in this order; k8s/README.md has the wait-for-Redis pattern)
kubectl apply -f k8s/

# Watch
curl http://<node-ip>:30080/grid
curl http://<node-ip>:30080/grid?format=json
kubectl logs -n gol deploy/perceiver -f
kubectl get jobs -n gol -l app=gol-agent

# Reset
kubectl delete jobs -n gol -l app=gol-agent
kubectl exec -n gol deploy/redis -- redis-cli FLUSHDB
```

No test suite exists in either Go module.

## Config plumbing

- `k8s/configmap.yaml` (`gol-config`) holds `grid_width`, `grid_height`, `tick_interval`. Both agent and perceiver pods pull these via `valueFrom: configMapKeyRef`. Changes apply to **new** Jobs only — running agents keep their original env until they die.
- The agent reads its own image name from `AGENT_IMAGE` so spawned children inherit it without hardcoding. When changing the registry/tag, update it in the seed Job, the perceiver Deployment, and the inline Job template inside both `spawnCell` functions.
- The perceiver's seed patterns are configured via the multi-line `SEED_PATTERNS` env var in `k8s/perceiver.yaml` (format: `name=x,y;x,y|name=x,y;...`).

## Image registry

All manifests reference `localhost:5000/gol-agent:latest` and `localhost:5000/gol-perceiver:latest` — local-only. Several YAML files carry comments noting that this needs to change when the cluster moves off the laptop.
