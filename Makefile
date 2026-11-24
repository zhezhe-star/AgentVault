.PHONY: build test vet fmt lint run deploy serve-dev stop-dev

# The current branch guards the production path: only the main checkout may
# replace the live proxy on PROD_PORT. Dev worktrees must test against
# DEV_PORT with their own data dir (serve-dev), never against production.
BRANCH    := $(shell git branch --show-current)
PROD_PORT := 8787
DEV_PORT  := 8788
PROD_HOME := $(HOME)/.config/agentvault
DEV_HOME  := $(HOME)/.config/agentvault-dev

build:
	go build -o bin/agentvault ./cmd/agentvault

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -w .

lint: fmt vet test

run: build
	./bin/agentvault --help

# Production upgrade — main branch only. Builds and replaces the live proxy
# on PROD_PORT using this checkout's binary.
deploy: build
	@if [ "$(BRANCH)" != "main" ]; then \
		echo "==> 拒绝部署:生产升级只允许在 main 分支(当前: $(BRANCH));开发分支请用 make serve-dev (端口 $(DEV_PORT))" >&2; \
		exit 1; \
	fi
	@kill $$(cat $(PROD_HOME)/proxy.pid) 2>/dev/null || true
	@sleep 1
	./bin/agentvault serve --addr 127.0.0.1:$(PROD_PORT) --daemon
	@sleep 1
	./bin/agentvault status

# Dev instance — test port with an isolated data dir (own pid file and DB
# copy; keychain secrets are namespaced globally and resolve in both). Safe
# to run from any worktree; it never touches the production proxy.
serve-dev: build
	@mkdir -p $(DEV_HOME)
	@if [ ! -f $(DEV_HOME)/agentvault.db ]; then \
		sqlite3 $(PROD_HOME)/agentvault.db ".backup '$(DEV_HOME)/agentvault.db'"; \
		echo "==> 已从生产库播种开发实例 DB(钥匙链全局共享,key 无需重录)"; \
	fi
	@AGENTVAULT_HOME=$(DEV_HOME) ./bin/agentvault serve --addr 127.0.0.1:$(DEV_PORT) --daemon

stop-dev:
	@kill $$(cat $(DEV_HOME)/proxy.pid) 2>/dev/null || true
	@rm -f $(DEV_HOME)/proxy.pid
