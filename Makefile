.PHONY: build test vet lint clean install

BINARY := deployment-manager
BUILD_DIR := dist
GOFLAGS := -trimpath
LDFLAGS := -s -w -X main.version=$(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

build:
	go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/$(BINARY) ./cmd/agent/

# Build for Linux AMD64 (VPS target)
build-linux:
	GOOS=linux GOARCH=amd64 go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/$(BINARY) ./cmd/agent/

test:
	go test -race -count=1 ./...

vet:
	go vet ./...

clean:
	rm -rf $(BUILD_DIR)

# Install to VPS (requires SSH access and the binary already built)
# Usage: make deploy-binary HOST=user@vps-ip
deploy-binary: build-linux
	scp $(BUILD_DIR)/$(BINARY) $(HOST):/tmp/$(BINARY)
	ssh $(HOST) 'sudo install -o root -g root -m 755 /tmp/$(BINARY) /opt/gymportal/agent/$(BINARY) && sudo systemctl restart deployment-manager'
