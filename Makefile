ENV ?= dev
CONFIG_DIR = config
SETTINGS_DIR = $(CONFIG_DIR)/settings
OUTPUT_FILE = krakend.json

PLUGIN_BUILD_DIR = plugins/build
BUILDER_IMAGE = krakend/builder:2.13.4
LOCAL_BUILDER_IMAGE = codehunters-plugin-builder:local
KRAKEND_IMAGE = krakend:2.13.4

PLUGINS = jwt-headers trace-context accept-language gateway-timeout session-resolver

CERTS_DIR = certs
TLS_CN ?= localhost
TLS_DAYS ?= 365

GEN_DIR = cmd/gen
ENDPOINTS_SPEC = endpoints.yaml
ENDPOINTS_JSON = $(SETTINGS_DIR)/endpoints.json

# Consumed by gen/check to filter which products are included in endpoints.json.
# Empty (default) includes all products. To load a subset locally, run:
#   make gen PRODUCTS=a,b
# Then start the stack:
#   make dev
# Note: make dev does not regenerate endpoints.json; it uses what's on disk.
PRODUCTS ?=

.PHONY: help check run build generate gen gen-check clean plugin-build plugin-check plugins-test plugins-abi jwt-issuer settings-check up down logs dev builder tls-dev-cert tls-clean

help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-20s\033[0m %s\n", $$1, $$2}'

gen: ## Regenerate $(ENDPOINTS_JSON) from $(ENDPOINTS_SPEC) (PRODUCTS= filters)
	@cd $(GEN_DIR) && go run . $(if $(PRODUCTS),"-products=$(PRODUCTS)",) "$(CURDIR)/$(ENDPOINTS_SPEC)" "$(CURDIR)/$(ENDPOINTS_JSON)"

gen-check: ## Fail if endpoints.json is out of sync with endpoints.yaml (refuses to run with PRODUCTS set)
	@if [ -n "$(PRODUCTS)" ]; then \
		echo "gen-check: refusing to run with PRODUCTS=$(PRODUCTS)."; \
		echo "  The committed endpoints.json is always the full set, so a drift check"; \
		echo "  against a filtered build would always fail. Run 'make check' with no"; \
		echo "  PRODUCTS, or 'make gen' to restore the full file."; \
		exit 1; \
	fi
	@$(MAKE) --no-print-directory gen
	@git diff --exit-code $(ENDPOINTS_JSON)

check: gen-check ## Validate KrakenD configuration (regen + drift + config guards + schema)
	@./scripts/check-jwt-single-issuer.sh
	@./scripts/check-settings-orphan-keys.sh
	@FC_ENABLE=1 \
	FC_SETTINGS="$(SETTINGS_DIR)" \
	./scripts/krakend-check.sh "$(CONFIG_DIR)/krakend.tmpl"
	@./scripts/check-plugin-chain-order.sh

generate: ## Generate the final krakend.json from templates
	@FC_ENABLE=1 \
	FC_SETTINGS="$(SETTINGS_DIR)" \
	FC_OUT="$(OUTPUT_FILE)" \
	./scripts/krakend-check.sh "$(CONFIG_DIR)/krakend.tmpl"
	@echo "Generated $(OUTPUT_FILE) with ENV=$(ENV)"

run: ## Run KrakenD locally with flexible configuration
	@FC_ENABLE=1 \
	FC_SETTINGS="$(SETTINGS_DIR)" \
	krakend run -c "$(CONFIG_DIR)/krakend.tmpl"

builder: ## Build the local plugin builder image (native arch)
	docker build -t $(LOCAL_BUILDER_IMAGE) -f plugins/Dockerfile.builder .

plugin-build: builder ## Build all plugins using Docker (native arch)
	@mkdir -p $(PLUGIN_BUILD_DIR)
	@for plugin in $(PLUGINS); do \
		echo "Building $$plugin..."; \
		docker run --rm \
			-v "$(CURDIR)/plugins/$$plugin:/app" \
			-w /app \
			$(LOCAL_BUILDER_IMAGE) \
			go build -buildmode=plugin -o /app/$$plugin.so . && \
		mv plugins/$$plugin/$$plugin.so $(PLUGIN_BUILD_DIR)/$$plugin.so && \
		echo "  $$plugin.so built successfully"; \
	done
	@echo "All plugins built at $(PLUGIN_BUILD_DIR)/"

plugin-check: ## Verify plugins load correctly
	docker run --rm \
		-v "$(CURDIR)/$(PLUGIN_BUILD_DIR):/opt/krakend/plugins" \
		-v "$(CURDIR)/$(CONFIG_DIR):/etc/krakend" \
		-e FC_ENABLE=1 -e FC_SETTINGS=/etc/krakend/settings \
		$(KRAKEND_IMAGE) check -d -t -c "/etc/krakend/krakend.tmpl"

plugins-test: ## Run go test + go vet across every plugin module
	@fail=0; \
	for plugin in $(PLUGINS); do \
		printf '%-18s ' "$$plugin"; \
		if (cd plugins/$$plugin && go test ./... >/tmp/pt.$$plugin.log 2>&1 && go vet ./... >>/tmp/pt.$$plugin.log 2>&1); then \
			echo "ok"; \
		else \
			echo "FAIL"; cat /tmp/pt.$$plugin.log; fail=1; \
		fi; \
		rm -f /tmp/pt.$$plugin.log; \
	done; \
	if [ $$fail -ne 0 ]; then echo "plugins-test: FAILED" >&2; exit 1; fi; \
	echo "plugins-test: OK ($(words $(PLUGINS)) modules)"

plugins-abi: ## Fail if the plugin builder's Go version drifts from the KrakenD image
	./scripts/check-plugin-abi.sh $(KRAKEND_IMAGE)

jwt-issuer: ## Fail if the edge declares anything other than exactly one issuer
	./scripts/check-jwt-single-issuer.sh

settings-check: ## Fail if config/settings declares a key the template never reads
	./scripts/check-settings-orphan-keys.sh

dev: plugin-build up ## Build plugins and start locally (full local dev)

build: ## Build Docker image (production)
	docker build -t codehunters-gw-krakend .

up: ## Start with docker-compose
	docker compose up -d

down: ## Stop docker-compose services
	docker compose down

logs: ## Show docker-compose logs
	docker compose logs -f krakend

clean: ## Remove generated files
	rm -f $(OUTPUT_FILE)
	rm -rf $(PLUGIN_BUILD_DIR)

tls-dev-cert: ## Generate self-signed cert for local HTTPS (CN=$(TLS_CN), $(TLS_DAYS)d)
	@mkdir -p $(CERTS_DIR)
	openssl req -x509 -newkey rsa:4096 -nodes \
		-keyout $(CERTS_DIR)/server.key \
		-out $(CERTS_DIR)/server.crt \
		-days $(TLS_DAYS) \
		-subj "/CN=$(TLS_CN)"
	@chmod 600 $(CERTS_DIR)/server.key
	@echo "Self-signed cert at $(CERTS_DIR)/server.{crt,key}. Set tls.disabled=false to enable."

tls-clean: ## Remove generated TLS certs (keeps .gitkeep)
	@find $(CERTS_DIR) -mindepth 1 ! -name '.gitkeep' -delete
	@echo "Cleared $(CERTS_DIR)/"
