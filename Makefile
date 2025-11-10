# Load .env if it exists
ifneq (,$(wildcard .env))
    include .env
    export
endif

SSH     = ssh -i $(SSH_KEY) $(USER)@$(IP)
RSYNC 	= rsync -avz -e "ssh -i $(SSH_KEY)" --rsync-path="sudo rsync"
REMOTE  = $(USER)@$(IP)

.PHONY: help all deps lint test build deploy clean

help:  # Show this help
	@echo "Targets:"
	@awk 'BEGIN {FS = ":.*#"; printf ""} /^[a-zA-Z0-9_%-]+:.*?#/ { printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2 }' $(MAKEFILE_LIST)

all: deploy  # Run deploy

deps:  # Install go dependencies
	go mod tidy

lint: deps  # Run linter
	golangci-lint run || true

test: lint  # Run unit and integration tests
	go test -race -coverprofile=coverage.out ./...
	@rm coverage.out

build: test # Build a statically linked binary
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -ldflags="-s -w" -o $(APP)

deploy: build  # Render a service file and deploy to remote server
	# Stop service (remote sudo)
	$(SSH) "sudo systemctl stop $(APP) || true"

	# Render service file locally
	@sed "s/CHANGE_ME_1/$(APP)/g; s/CHANGE_ME_2/$(TOKEN)/g; w $(APP).service" service.tpl > /dev/null

	# Sync binary to /root (remote rsync runs under sudo)
	$(RSYNC) ./$(APP) $(REMOTE):/root/$(APP)

	# Sync service file to systemd dir
	$(RSYNC) ./$(APP).service $(REMOTE):/etc/systemd/system/$(APP).service

	# Fix permissions and restart service
	$(SSH) "sudo chmod +x /root/$(APP)"
	$(SSH) "sudo systemctl daemon-reload"
	$(SSH) "sudo systemctl enable $(APP)"
	$(SSH) "sudo systemctl restart $(APP)"

clean:  # Clean local and remote files
	# Local cleanup
	rm -f ./$(APP) ./$(APP).service

	# Remote cleanup (all via sudo)
	$(SSH) "sudo systemctl stop $(APP) || true"
	$(SSH) "sudo systemctl disable $(APP) || true"
	$(SSH) "sudo rm -f /etc/systemd/system/$(APP).service /root/$(APP)"
	$(SSH) "sudo systemctl daemon-reload"
