VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build run test vet install uninstall install-user uninstall-user

build:
	go build -ldflags "$(LDFLAGS)" -o bin/alexaproxy ./cmd/alexaproxy

run: build
	./bin/alexaproxy serve

vet:
	go vet ./...

test:
	go test -race ./...

# Installs the binary and systemd unit, then (re)starts the service.
install: build
	sudo install -m 0755 bin/alexaproxy /usr/local/bin/alexaproxy
	sudo install -m 0644 deploy/alexaproxy.service /etc/systemd/system/alexaproxy.service
	sudo systemctl daemon-reload
	sudo systemctl enable --now alexaproxy
	sudo systemctl restart alexaproxy

uninstall:
	-sudo systemctl disable --now alexaproxy
	sudo rm -f /etc/systemd/system/alexaproxy.service /usr/local/bin/alexaproxy
	sudo systemctl daemon-reload

# Same as install, but as a systemd user service (no root). Credentials stay
# in ~/.config/alexaproxy.
install-user: build
	install -D -m 0755 bin/alexaproxy $(HOME)/.local/bin/alexaproxy
	install -D -m 0644 deploy/alexaproxy-user.service $(HOME)/.config/systemd/user/alexaproxy.service
	systemctl --user daemon-reload
	systemctl --user enable --now alexaproxy
	systemctl --user restart alexaproxy
	@loginctl show-user $(USER) -p Linger | grep -q yes || echo "note: run 'loginctl enable-linger $(USER)' to start at boot"

uninstall-user:
	-systemctl --user disable --now alexaproxy
	rm -f $(HOME)/.config/systemd/user/alexaproxy.service $(HOME)/.local/bin/alexaproxy
	systemctl --user daemon-reload
