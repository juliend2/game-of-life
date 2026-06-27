build:
	go build ./...

deps:
	go mod tidy

docker:
	docker build -t localhost:5000/gol-perceiver:latest .
	docker push localhost:5000/gol-perceiver:latest

