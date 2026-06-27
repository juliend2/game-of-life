# Instructions for K3s on my laptop

## k3s on Manjaro

k3s runs fine on Manjaro/Arch. A few things to know upfront:

- k3s writes its kubeconfig to `/etc/rancher/k3s/k3s.yaml` (root-owned)
- it runs as a systemd service
- it bundles its own `kubectl` as `k3s kubectl`, but you likely already have `kubectl` installed or can symlink it

---

## Step 1 — Install k3s

```bash
curl -sfL https://get.k3s.io | sh -
```

This installs k3s and starts it as a systemd service immediately. Check it:

```bash
sudo systemctl status k3s
sudo k3s kubectl get nodes
```

You should see your machine as a single `Ready` node.

---

## Step 2 — Configure kubectl for your user

The kubeconfig is root-only by default. Fix that:

```bash
# Copy it somewhere your user owns:
mkdir -p ~/.kube
sudo cp /etc/rancher/k3s/k3s.yaml ~/.kube/config-k3s
sudo chown $USER:$USER ~/.kube/config-k3s
chmod 600 ~/.kube/config-k3s

# Point kubectl at it — add this to your ~/.bashrc or ~/.zshrc:
export KUBECONFIG=~/.kube/config-k3s
```

Then reload your shell and test:

```bash
source ~/.bashrc   # or ~/.zshrc
kubectl get nodes
```

---

## Step 3 — Set up a local registry

Rather than importing tar files every time, a local registry makes the dev loop much faster. Run it as a plain Docker container:

```bash
docker run -d \
  --name registry \
  --restart=always \
  -p 5000:5000 \
  registry:2
```

Then tell k3s to trust it without TLS. Create this file:

```bash
sudo mkdir -p /etc/rancher/k3s
sudo tee /etc/rancher/k3s/registries.yaml << 'EOF'
mirrors:
  "localhost:5000":
    endpoint:
      - "http://localhost:5000"
EOF
```

Restart k3s to pick it up:

```bash
sudo systemctl restart k3s
```

And tell Docker to also trust it as insecure. Add to `/etc/docker/daemon.json` (create it if it doesn't exist):

```json
{
  "insecure-registries": ["localhost:5000"]
}
```

Restart Docker:

```bash
sudo systemctl restart docker
```

---

## Step 4 — Your dev loop

Once you have Go images to build, the workflow will be:

```bash
# Build and push to local registry:
docker build -t localhost:5000/gol-agent:latest ./agent
docker push localhost:5000/gol-agent:latest

# k3s pulls from localhost:5000 transparently
```

And in your manifests, replace `your-registry/` with `localhost:5000/`:

```yaml
image: localhost:5000/gol-agent:latest
imagePullPolicy: Always
```

---

## Step 5 — Apply the manifests

```bash
kubectl apply -f k8s/namespace.yaml
kubectl apply -f k8s/rbac.yaml
kubectl apply -f k8s/configmap.yaml
kubectl apply -f k8s/redis.yaml

# Wait for Redis:
kubectl wait --namespace gol \
  --for=condition=ready pod \
  --selector=app=redis \
  --timeout=60s

kubectl apply -f k8s/perceiver.yaml
```

Check everything came up:

```bash
kubectl get all -n gol
```

You should see:

```
NAME                             READY   STATUS    RESTARTS
pod/redis-xxxx                   1/1     Running   0
pod/perceiver-xxxx               0/1     Pending   0   ← image not built yet, normal
```

---

## Useful commands to keep handy

```bash
# Watch pods live:
kubectl get pods -n gol -w

# Perceiver logs:
kubectl logs -n gol deploy/perceiver -f

# Redis CLI from inside the cluster:
kubectl exec -n gol deploy/redis -- redis-cli KEYS '*'

# Nuke everything and start fresh (keeps k3s running):
kubectl delete namespace gol
kubectl apply -f k8s/
```

## Stopping and starting k3s

```bash
# Stop (frees all RAM — useful if you're not working on the project):
sudo systemctl stop k3s

# Start again:
sudo systemctl start k3s

# Disable autostart on boot:
sudo systemctl disable k3s
```

---

That's the full local setup. Once the cluster is up and Redis is running, you're ready to write Go. Want to start with the agent or the perceiver?
