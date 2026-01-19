.PHONY: all build agent server test clean fmt vet

all: build

build: agent server

agent:
	go build -o bin/gpu-agent ./src/agent

server:
	go build -o bin/gpu-server ./src/server

test:
	go test -v ./...

clean:
	rm -rf bin/
	go clean

fmt:
	go fmt ./...

vet:
	go vet ./...

# Cross-compilation targets
agent-linux:
	GOOS=linux GOARCH=amd64 go build -o bin/gpu-agent-linux-amd64 ./src/agent

agent-darwin:
	GOOS=darwin GOARCH=arm64 go build -o bin/gpu-agent-darwin-arm64 ./src/agent

agent-windows:
	GOOS=windows GOARCH=amd64 go build -o bin/gpu-agent-windows-amd64.exe ./src/agent
