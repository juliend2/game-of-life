# GoL Agent (cell)

## What the agent does

```
1. Start up — read CELL_X, CELL_Y from env
2. Register itself in Redis  →  SET cell:X:Y 1
3. Loop every TICK_INTERVAL seconds:
     a. Count alive neighbors  (check Redis for the 8 surrounding cells)
     b. Apply GoL rules:
          < 2 alive neighbors  →  die (underpopulation)
          > 3 alive neighbors  →  die (overpopulation)
          2 or 3               →  survive
          dead neighbor with exactly 3 alive neighbors  →  spawn a new Job there
     c. For each cell to spawn → create a Job via kube API
     d. If dying → deregister from Redis, exit(0)
4. On exit (any reason) → deregister from Redis
```

---

## Project structure

```
gol/
├── k8s/
│   └── ...                  ← already done
└── agent/
    ├── Dockerfile
    ├── go.mod
    ├── go.sum
    └── main.go
```

---

## The codeGood — the Go proxy is blocked in this sandbox, so I'll write all the code directly and you'll run `go mod tidy` on your machine. Let me write the full agent now.Now the Dockerfile:---

## To get it running on your machine

```bash
cd gol/agent

# Pull dependencies (needs internet access, unlike this sandbox):
go mod tidy

# Make sure it compiles:
go build ./...

# Build and push the image to your local registry:
docker build -t localhost:5000/gol-agent:latest .
docker push localhost:5000/gol-agent:latest
```

---

## A few things worth noting in the code

**The SETNX lock in `spawnCell`** — when two neighboring cells both survive a tick, they might both try to spawn the same new cell. Redis's `SETNX` (set if not exists) is atomic, so only one of them wins the lock. The other backs off silently. No duplicate Jobs.

**The `defer deregister`** — this runs no matter how the agent exits: normal death by GoL rules, SIGTERM from Kubernetes, OOMKill, or a panic. The cell always removes itself from Redis on the way out. This keeps the registry clean.

**`AGENT_IMAGE` env var** — when an agent spawns a child Job, it needs to know which image to use. Rather than hardcoding it, it reads its own image name from an env var. This means you can change the image tag in one place (the seed Job) and all children inherit it automatically.

**`rest.InClusterConfig()`** — this is how a pod authenticates to the Kubernetes API from inside the cluster. It reads the ServiceAccount token that Kubernetes automatically mounts at `/var/run/secrets/kubernetes.io/serviceaccount/`. No kubeconfig file needed inside the container.

