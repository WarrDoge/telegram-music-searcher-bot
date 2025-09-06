TOKEN := $(TELEGRAM_BOT_TOKEN)
IP := $(SERVER_IP)
APP=music-searcher

.PHONY: all deps lint build deploy help

help:
	@echo "Available targets:"
	@echo "  all    - Build and deploy (default)"
	@echo "  deps   - Fetch Go dependencies"
	@echo "  lint 	- Run golangci-lint"
	@echo "  build 	- Compile static Go binary"
	@echo "  deploy - Deploy unit and binary to remote target"
	@echo "  clean 	- Remove local and remote binaries"
	@echo "  help   - Show this help message"

all: deploy

deps:
	go mod tidy

lint: deps
	golangci-lint run

build:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o $(APP)

deploy: build
	ssh root@$(IP) "systemctl stop $(APP)"

	@sed "s/CHANGE_ME_1/$(APP)/g; s/CHANGE_ME_2/$(TOKEN)/g; w $(APP).service" $(APP).service.tpl >/dev/null

	scp ./$(APP) root@$(IP):/root/
	scp ./$(APP).service root@$(IP):/etc/systemd/system/

	ssh root@$(IP) "chmod +x /root/$(APP)"
	ssh root@$(IP) "systemctl enable $(APP)"
	ssh root@$(IP) "systemctl start $(APP)"

clean:
	rm ./$(APP) ./$(APP).service

	ssh root@$(IP) "systemctl stop $(APP)"
	ssh root@$(IP) "systemctl disable $(APP)"
	ssh root@$(IP) "rm -f /etc/systemd/system/$(APP).service"
	ssh root@$(IP) "rm -f /root/$(APP)"
