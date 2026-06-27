REGISTRY ?= localhost:5000
AGENT_IMAGE ?= $(REGISTRY)/gol-agent:latest
PERCEIVER_IMAGE ?= $(REGISTRY)/gol-perceiver:latest

.PHONY: build build-agent build-perceiver \
				docker docker-agent docker-perceiver \
				push push-agent push-perceiver \
				deps tidy deploy

build: build-agent build-perceiver

build-agent:
	go build -o agent/agent ./agent

build-perceiver:
	go build -o perceiver/perceiver ./perceiver

docker: docker-agent docker-perceiver

docker-agent:
	docker build -f agent/Dockerfile -t $(AGENT_IMAGE) .

docker-perceiver:
	docker build -f perceiver/Dockerfile -t $(PERCEIVER_IMAGE) .

push: push-agent push-perceiver

push-agent:
	docker push $(AGENT_IMAGE)

push-perceiver:
	docker push $(PERCEIVER_IMAGE)

deps tidy:
	go mod tidy

deploy: docker push
	kubectl rollout restart deployment/perceiver -n gol
	kubectl rollout status deployment/perceiver -n gol --timeout=60s
	kubectl delete jobs -n gol -l app=gol-agent


