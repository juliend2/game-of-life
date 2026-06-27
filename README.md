# Game of Life, with Go & Kubernetes

## Dev

when you make a change, do this to build the image and push it, and deploy it:

```bash
make docker push
kubectl rollout restart deployment/perceiver -n gol
kubectl delete jobs -n gol -l app=gol-agent
```

## Watch the game

```bash
watch -n 5 'curl -s http://localhost:30080/grid'
```


