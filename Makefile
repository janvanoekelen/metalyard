.PHONY: all build agent server test clean fmt vet

all: build

build: agent server

agent:
	cd src/agent && go build -o ../../bin/gpu-agent .

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
	cd src/agent && GOOS=linux GOARCH=amd64 go build -o ../../bin/gpu-agent-linux-amd64 .

agent-darwin:
	cd src/agent && GOOS=darwin GOARCH=arm64 go build -o ../../bin/gpu-agent-darwin-arm64 .

agent-windows:
	cd src/agent && GOOS=windows GOARCH=amd64 go build -o ../../bin/gpu-agent-windows-amd64.exe .
