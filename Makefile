# Entry point for the devnet. `make up` is the whole story; everything else is either a
# step of it or the container-free variant.
#
# ENCLAVE and ARGS are overridable: `make up ENCLAVE=pbt2 ARGS=args/mine.yaml`.

ENCLAVE ?= pbt
BLOCKS  ?= 100
ARGS    ?= args/devnet.yaml

.DEFAULT_GOAL := help
.PHONY: help up down logs build besu besu-image bin genesis check

help:
	@echo "pbt-devnet — a differential test harness for EIP-8297 execution clients"
	@echo ""
	@grep -hE '^[a-z][a-z-]*:.*## ' $(MAKEFILE_LIST) \
	  | sed 's/:.*## /\t/' \
	  | awk -F'\t' '{printf "  make %-8s %s\n", $$1, $$2}'
	@echo ""
	@echo "  ENCLAVE=$(ENCLAVE)  ARGS=$(ARGS)"

fast: ## two clients, minimal traffic, no extra tooling — a four-minute debug loop
	@$(MAKE) --no-print-directory up ARGS=args/fast.yaml BLOCKS=40

up: check build ## build the images and start the devnet, then follow the monitor
	@kurtosis enclave rm -f $(ENCLAVE) >/dev/null 2>&1 || true
	@# --privileged is for disruptoor only: it enters other containers' network namespaces
	@# to apply partitions and latency, so it needs NET_ADMIN, the docker socket and the
	@# host PID namespace. Kurtosis gates all three behind one per-run opt-in. Drop
	@# disruptoor from additional_services and this flag goes with it.
	kurtosis run . --enclave $(ENCLAVE) --args-file $(ARGS) --privileged
	@echo ""
	@echo "==> following pbtmonitor. Ctrl-C detaches; the devnet keeps running."
	@echo "    'make down' stops it. Watch for lines beginning FINDING."
	@# Ctrl-C is how you leave this, so a non-zero exit here is the normal case.
	@kurtosis service logs $(ENCLAVE) pbtmonitor -f || true

down: ## stop and remove the devnet
	-@kurtosis enclave rm -f $(ENCLAVE)

logs: ## re-attach to the monitor
	kurtosis service logs $(ENCLAVE) pbtmonitor -f

ui: ## print every web UI and API url
	@printf '%-12s %s\n' \
	  dora       "$$(kurtosis port print $(ENCLAVE) dora http 2>/dev/null)" \
	  spamoor    "$$(kurtosis port print $(ENCLAVE) spamoor http 2>/dev/null)" \
	  assertoor  "$$(kurtosis port print $(ENCLAVE) assertoor http 2>/dev/null)" \
	  forky      "$$(kurtosis port print $(ENCLAVE) forky http 2>/dev/null)" \
	  disruptoor "$$(kurtosis port print $(ENCLAVE) disruptoor http 2>/dev/null)"

status: ## show every execution client's head and state root
	@scripts/status.sh $(ENCLAVE)

verify: ## compare every client at the same block number (BLOCKS=100)
	@scripts/verify.py $(ENCLAVE) --blocks $(BLOCKS) --wait

diagnose: ## where did the chain split, and what were the peers doing then
	@scripts/diagnose.py $(ENCLAVE)

split: ## partition the network: participants 1,2 | 3  (el and cl)
	@scripts/chaos.sh $(ENCLAVE) split

heal: ## remove every partition and shaping rule
	@scripts/chaos.sh $(ENCLAVE) heal

chaos: ## split, hold, heal, and report whether the branches actually diverged
	@scripts/chaos.sh $(ENCLAVE) cycle

build: ## build the geth image, the genesis generator, the monitor and the hammer
	scripts/build-images.sh $(ARGS)

besu: ## build besu-pbt:local (two Gradle stages, then the image; needs JDK 25)
	scripts/build-besu.sh

besu-image: ## rebuild besu-pbt:local from an existing build/install/besu
	scripts/build-besu.sh --skip-gradle

bin: ## build the driver and hammer as host binaries into bin/
	@mkdir -p bin
	cd monitor && go build -o ../bin/pbtmonitor .
	cd hammer  && go build -o ../bin/pbthammer .
	@echo "==> bin/pbtmonitor bin/pbthammer"

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
