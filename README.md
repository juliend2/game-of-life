# Game of Life, with Go & Kubernetes

## Dev

when you make a change, do this to build the image and push it, and deploy it:

```bash
# make your changes
make deploy # will build and push the image, and restart the jobs
```

## Watch the game

```bash
watch -n 5 'curl -s http://localhost:30080/grid'
```


