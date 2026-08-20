# Entry point for the devnet. `make up` builds the images and starts it; everything else
# either builds a piece of it or asks the running network a question.
#
# ENCLAVE and ARGS are overridable: `make up ENCLAVE=pbt2 ARGS=args/mine.yaml`.

ENCLAVE ?= pbt
BLOCKS  ?= 100
ARGS    ?= args/devnet.yaml
NAME    ?= code-shared
DEPTH   ?= 10
# Empty means "next node in pbtchaos's rotation", which is what spreads reorgs across
# both client types.
MINORITY ?=

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
	  disruptoor "$$(kurtosis port print $(ENCLAVE) disruptoor http 2>/dev/null)" \
	  pbtchaos   "$$(kurtosis port print $(ENCLAVE) pbtchaos http 2>/dev/null)"

status: ## show every execution client's head and state root
	@scripts/pbt.py status $(ENCLAVE)

verify: ## compare every client at the same block number (BLOCKS=100)
	@scripts/pbt.py verify $(ENCLAVE) --blocks $(BLOCKS) --wait

forks: ## competing heads, how deep each branch is, and who is on which
	@scripts/pbt.py forks $(ENCLAVE)

proposals: ## who was due to propose each slot, and who missed
	@scripts/pbt.py proposals $(ENCLAVE)

diagnose: ## where did the chain split, and what were the peers doing then
	@scripts/pbt.py diagnose $(ENCLAVE)

split: ## partition the network by hand: majority | last participant (el and cl)
	@scripts/pbt.py split $(ENCLAVE)

heal: ## remove every partition and shaping rule
	@scripts/pbt.py heal $(ENCLAVE)

repeer: ## restart any consensus client left with no peers after a partition
	@scripts/pbt.py repeer $(ENCLAVE)

chaos-status: ## what pbtchaos is doing now, what is queued, and recent results
	@scripts/pbt.py chaos-status $(ENCLAVE)

scenario: ## run one reorg scenario (NAME=code-shared DEPTH=20 [MINORITY=3])
	@scripts/pbt.py scenario $(ENCLAVE) $(NAME) $(DEPTH) $(MINORITY)

build: ## build every local image the package expects
	scripts/build-images.sh $(ARGS)

besu: ## build besu-pbt:local (two Gradle stages, then the image; needs JDK 25)
	scripts/build-besu.sh

besu-image: ## rebuild besu-pbt:local from an existing build/install/besu
	scripts/build-besu.sh --skip-gradle

bin: ## build every command as a host binary into bin/
	@mkdir -p bin
	go build -o bin/ ./cmd/...
	@echo "==> $$(ls bin | tr '\n' ' ')"

genesis: ## regenerate genesis/genesis.json and print its root (for single-client debugging)
	@mkdir -p bin
	go build -o bin/gengenesis ./cmd/gengenesis
	./bin/gengenesis --out genesis/genesis.json --gaslimit 200000000

check: ## verify docker and kurtosis are present
	@command -v docker >/dev/null 2>&1 || { \
	  echo "docker not found. Install Docker Desktop, or OrbStack."; exit 1; }
	@docker info >/dev/null 2>&1 || { \
	  echo "docker is installed but not responding — start Docker and retry."; exit 1; }
	@command -v kurtosis >/dev/null 2>&1 || { \
	  echo "kurtosis not found:  brew install kurtosis-tech/tap/kurtosis-cli"; exit 1; }
	@echo "==> docker and kurtosis are both present"
