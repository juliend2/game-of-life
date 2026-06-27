# GoL on Kubernetes — manifests

## Files

```
k8s/
├── namespace.yaml      — the gol namespace
├── rbac.yaml           — ServiceAccounts + Roles for agent and perceiver
├── configmap.yaml      — grid size, tick interval
├── redis.yaml          — Redis pod, PVC, and ClusterIP Service
├── perceiver.yaml      — the HTTP dashboard pod + extinction watchdog + NodePort Service
└── seed.yaml           — the initial pattern + the agent Job template
```

## The Perceiver

Named in homage to Berkeley's *esse est percipi* — to be is to be perceived.
The perceiver is the only eternal pod in the simulation. Everything else is born
and dies. The perceiver watches, and when darkness falls (extinction), it speaks
existence back into being by re-seeding the grid with a randomly chosen pattern.

Extinction detection works as follows:

```
every poll_interval seconds:
    alive = count keys matching cell:* in Redis
    if alive == 0:
        silence_count++
        if silence_count >= EXTINCTION_GRACE_TICKS:
            pick a random pattern from SEED_PATTERNS
            create the seed Job
            silence_count = 0
    else:
        silence_count = 0
```

The grace period (`EXTINCTION_GRACE_TICKS`) prevents false positives during the
brief moment between one generation's deaths and the next generation's births.
Set it to at least `2 × tick_interval` to be safe.

## Deployment order

Apply in this order (dependencies flow downward):

```bash
kubectl apply -f k8s/namespace.yaml
kubectl apply -f k8s/rbac.yaml
kubectl apply -f k8s/configmap.yaml
kubectl apply -f k8s/redis.yaml

# Wait for Redis to be ready before starting anything else
kubectl wait --namespace gol \
  --for=condition=ready pod \
  --selector=app=redis \
  --timeout=60s

kubectl apply -f k8s/perceiver.yaml
kubectl apply -f k8s/seed.yaml   # this starts the simulation
```

Or all at once (Kubernetes will sort out the ordering on its own, just
be patient for the first few seconds):

```bash
kubectl apply -f k8s/
```

## Watching the simulation

```bash
# From any machine on your Tailscale network:
curl http://<node-tailscale-ip>:30080/grid

# Watch it refresh every 5 seconds in your terminal:
watch -n 5 'curl -s http://<node-tailscale-ip>:30080/grid'

# See all living cells (= running agent Jobs):
kubectl get jobs -n gol -l app=gol-agent

# Count living cells:
kubectl get jobs -n gol -l app=gol-agent --no-headers | wc -l

# See the perceiver logs (extinction events, re-seeds):
kubectl logs -n gol deploy/perceiver -f
```

## Resetting the simulation manually

The perceiver handles extinction automatically, but if you want to force a reset:

```bash
# Kill all agent Jobs (wipes the board)
kubectl delete jobs -n gol -l app=gol-agent

# Flush Redis (removes any stale cell keys)
kubectl exec -n gol deploy/redis -- redis-cli FLUSHDB

# The perceiver will detect extinction within EXTINCTION_GRACE_TICKS polls
# and re-seed automatically. Or force it immediately:
kubectl rollout restart deployment/perceiver -n gol
```

## Tuning

All simulation parameters live in `configmap.yaml`. After editing:

```bash
kubectl apply -f k8s/configmap.yaml
```

New agent Jobs will pick up the new values. Running agents keep their
current config until they exit and are replaced.

| Parameter     | Default | Notes                                           |
|---------------|---------|-------------------------------------------------|
| grid_width    | 20      | Max x coordinate (0-indexed)                    |
| grid_height   | 20      | Max y coordinate (0-indexed)                    |
| tick_interval | 10      | Seconds between GoL rule evaluations per agent  |

## Resource budget (approximate)

| Component      | RAM      | CPU (idle) |
|----------------|----------|------------|
| Redis          | ~64 MB   | ~0%        |
| Observer       | ~32 MB   | ~0%        |
| Agent pod (×N) | ~16 MB   | ~0%        |
| 100 agents     | ~1.6 GB  | ~0%        |
