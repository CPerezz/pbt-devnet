# Entry point for the devnet. `make up` is the whole story; everything else is either a
# step of it or the container-free variant.
#
# ENCLAVE and ARGS are overridable: `make up ENCLAVE=pbt2 ARGS=args/mine.yaml`.

ENCLAVE ?= pbt
ARGS    ?= args/devnet.yaml

.DEFAULT_GOAL := help
.PHONY: help up down logs build bin dev genesis check

help:
	@echo "pbt-devnet — a differential test harness for EIP-8297 execution clients"
	@echo ""
	@grep -hE '^[a-z][a-z-]*:.*## ' $(MAKEFILE_LIST) \
	  | sed 's/:.*## /\t/' \
	  | awk -F'\t' '{printf "  make %-8s %s\n", $$1, $$2}'
	@echo ""
	@echo "  ENCLAVE=$(ENCLAVE)  ARGS=$(ARGS)"

up: check build ## build the images and start the devnet, then follow the monitor
	@kurtosis enclave rm -f $(ENCLAVE) >/dev/null 2>&1 || true
	@# --privileged is for disruptoor only: it enters other containers' network namespaces
	@# to apply partitions and latency, so it needs NET_ADMIN, the docker socket and the
	@# host PID namespace. Kurtosis gates all three behind one per-run opt-in. Drop
	@# disruptoor from additional_services and this flag goes with it.
	kurtosis run . --enclave $(ENCLAVE) --args-file $(ARGS) --privileged
	@echo ""
	@echo "==> following pbtdriver. Ctrl-C detaches; the devnet keeps running."
	@echo "    'make down' stops it. Watch for lines beginning FINDING."
	@# Ctrl-C is how you leave this, so a non-zero exit here is the normal case.
	@kurtosis service logs $(ENCLAVE) pbtmonitor -f || true

down: ## stop and remove the devnet (both the enclave and any host-mode run)
	-@kurtosis enclave rm -f $(ENCLAVE)
	-@scripts/local-devnet.sh stop 2>/dev/null

logs: ## re-attach to the driver
	kurtosis service logs $(ENCLAVE) pbtmonitor -f

build: ## build the geth image, the genesis generator, the monitor and the hammer
	scripts/build-images.sh $(ARGS)

besu: ## build besu-pbt:local (two Gradle stages, then the image; needs JDK 25)
	scripts/build-besu.sh

besu-image: ## rebuild besu-pbt:local from an existing build/install/besu
	scripts/build-besu.sh --skip-gradle

bin: ## build the driver and hammer as host binaries into bin/
	@mkdir -p bin
	cd driver  && go build -o ../bin/pbtdriver .
	cd hammer  && go build -o ../bin/pbthammer .
	@echo "==> bin/pbtdriver bin/pbthammer"

dev: bin ## container-free loop: geth processes on the host, no Docker or Kurtosis
	@test -x bin/pbtgeth || { \
	  echo "no geth binary at bin/pbtgeth. Build one from the checkout the args file names:"; \
	  echo "    cd $$(yq -r '.client_sources.geth.path // \"../go-ethereum\"' $(ARGS) 2>/dev/null || echo ../go-ethereum)"; \
	  echo "    go build -o $(CURDIR)/bin/pbtgeth ./cmd/geth"; \
	  exit 1; }
	scripts/local-devnet.sh start
	scripts/local-devnet.sh driver

genesis: ## regenerate genesis/genesis.json and print the root to paste into the args file
	@mkdir -p bin
	cd gengenesis && go build -o ../bin/gengenesis .
	./bin/gengenesis --out genesis/genesis.json --gaslimit 200000000

check: ## verify docker, kurtosis and a yaml reader are present
	@command -v docker >/dev/null 2>&1 || { \
	  echo "docker not found. Install Docker Desktop, or OrbStack."; exit 1; }
	@docker info >/dev/null 2>&1 || { \
	  echo "docker is installed but not responding — start Docker and retry."; exit 1; }
	@command -v kurtosis >/dev/null 2>&1 || { \
	  echo "kurtosis not found:  brew install kurtosis-tech/tap/kurtosis-cli"; exit 1; }
	@{ command -v yq >/dev/null 2>&1 || python3 -c 'import yaml' >/dev/null 2>&1; } || { \
	  echo "need a yaml reader to parse $(ARGS):  brew install yq   (or pip3 install pyyaml)"; exit 1; }
	@echo "==> docker, kurtosis and a yaml reader are all present"
