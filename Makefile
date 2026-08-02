ENV ?= dev
CONFIG_DIR = config
SETTINGS_DIR = $(CONFIG_DIR)/settings
OUTPUT_FILE = krakend.json

PLUGIN_BUILD_DIR = plugins/build
BUILDER_IMAGE = krakend/builder:2.13.4
LOCAL_BUILDER_IMAGE = krakend-plugin-builder:local
KRAKEND_IMAGE = krakend:2.13.4

PLUGINS = jwt-headers ip-resolver trace-context accept-language gateway-timeout

CERTS_DIR = certs
TLS_CN ?= localhost
TLS_DAYS ?= 365

.PHONY: help check run build generate clean plugin-build plugin-check up down logs dev builder tls-dev-cert tls-clean

help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-20s\033[0m %s\n", $$1, $$2}'

check: ## Validate KrakenD configuration
	@FC_ENABLE=1 \
	FC_SETTINGS="$(SETTINGS_DIR)" \
	krakend check -d -t -c "$(CONFIG_DIR)/krakend.tmpl"

generate: ## Generate the final krakend.json from templates
	@FC_ENABLE=1 \
	FC_SETTINGS="$(SETTINGS_DIR)" \
	FC_OUT="$(OUTPUT_FILE)" \
	krakend check -d -t -c "$(CONFIG_DIR)/krakend.tmpl"
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

dev: plugin-build up ## Build plugins and start locally (full local dev)

build: ## Build Docker image (production)
	docker build -t krakend-gateway .

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
