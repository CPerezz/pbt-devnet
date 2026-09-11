# Entry point for the devnet. Two scenarios: `make tree-at-genesis` (EIP-8297, the tree from
# block 0) and `make migration` (EIP-8347, the live switch). Everything else builds a piece
# of one or asks the running network a question.
#
# ENCLAVE and ARGS are overridable: `make up ENCLAVE=pbt2 ARGS=args/mine.yaml`.

ENCLAVE ?= pbt
BLOCKS ?= 100
ARGS ?= args/tree-at-genesis.yaml
NAME ?= code-shared
DEPTH ?= 10
# Empty means "next node in pbtchaos's rotation", which is what spreads reorgs across
# both client types.
MINORITY ?=
PINS ?= verify/pins.yaml
# No default: a per-run unix time with no sane guess. See `make judge`.
BINARY_TRIE_TIME ?=
LOGS_DIR ?=

.DEFAULT_GOAL := help
.PHONY: help tree-at-genesis migration migration-smoke up down logs ui ui-preview status \
        compare forks proposals diagnose split heal repeer chaos-status scenario judge \
        build sources bin genesis check

# `## <group>: <text>` on a target line documents it; help groups them in first-seen order.
help:
	@echo "pbt-devnet — differential devnets for EIP-8297 (binary tree) and EIP-8347 (migration)"
	@grep -hE '^[a-z][a-z-]*:.*## ' $(MAKEFILE_LIST) \
	  | sed -E 's/^([a-z-]+):.*## ([A-Za-z ]+): (.*)/\2\t\1\t\3/' \
	  | awk -F'\t' '$$1 != g {g = $$1; printf "\n%s\n", g} {printf "  make %-16s %s\n", $$2, $$3}'
	@echo ""
	@echo "  ENCLAVE=$(ENCLAVE)  ARGS=$(ARGS)"

tree-at-genesis: ## Scenarios: EIP-8297 - geth, besu and erigon on the tree from block 0, reorged on purpose
	$(MAKE) up ARGS=args/tree-at-genesis.yaml

migration: ## Scenarios: EIP-8347 - four geth switch to the tree at binaryTrieTime; one full lap, judged (~1 h)
	$(MAKE) lap ARGS=args/migration.yaml

migration-smoke: ## Scenarios: the short migration lap (~45 min)
	$(MAKE) lap ARGS=args/migration-smoke.yaml

up: check build ## Scenarios: start ARGS as a plain network and follow its monitor (no lap driver)
	@kurtosis enclave rm -f $(ENCLAVE) >/dev/null 2>&1 || true
	@# --privileged is for disruptoor only: it enters other containers' network namespaces
	@# to apply partitions and latency, so it needs NET_ADMIN, the docker socket and the
	@# host PID namespace. Kurtosis gates all three behind one per-run opt-in. Drop
	@# disruptoor from additional_services and this flag goes with it.
	kurtosis run . --enclave $(ENCLAVE) --args-file $(ARGS) --privileged
	@echo ""
	@echo "==> following the monitor. Ctrl-C detaches; the devnet keeps running."
	@echo "    'make down' stops it. Watch for lines beginning FINDING."
	@# Ctrl-C is how you leave this, so a non-zero exit here is the normal case.
	@$(MAKE) logs || true

lap: check build
	ENCLAVE=$(ENCLAVE) ARGS=$(ARGS) scripts/lap.sh

down: ## Scenarios: stop and remove the enclave
	-@kurtosis enclave rm -f $(ENCLAVE)

logs: ## Scenarios: re-attach to the monitor (the migration monitor when present, else pbtmonitor)
	@m=pbtmonitor; kurtosis enclave inspect $(ENCLAVE) 2>/dev/null | grep -q '\bmigration-monitor\b' && m=migration-monitor; \
	kurtosis service logs $(ENCLAVE) $$m -f

ui: ## Watching: print every web UI and API url
	@printf '%-12s %s\n' \
	  dora       "$$(kurtosis port print $(ENCLAVE) dora http 2>/dev/null)" \
	  spamoor    "$$(kurtosis port print $(ENCLAVE) spamoor http 2>/dev/null)" \
	  assertoor  "$$(kurtosis port print $(ENCLAVE) assertoor http 2>/dev/null)" \
	  disruptoor "$$(kurtosis port print $(ENCLAVE) disruptoor http 2>/dev/null)" \
	  pbtchaos   "$$(kurtosis port print $(ENCLAVE) pbtchaos http 2>/dev/null)" \
	  migration  "$$(kurtosis port print $(ENCLAVE) migration-monitor http 2>/dev/null)"

ui-preview: ## Watching: the migration monitor page on a synthetic lap (no enclave needed)
	@echo "==> http://127.0.0.1:8765/index.html?synthetic"
	@python3 -m http.server 8765 --bind 127.0.0.1 -d cmd/migration-monitor/ui

status: ## Watching: every execution client's head and state root
	@scripts/pbt.py status $(ENCLAVE)

compare: ## Watching: the same state root on every client at the same block (BLOCKS=100)
	@scripts/pbt.py verify $(ENCLAVE) --blocks $(BLOCKS) --wait

forks: ## Watching: competing heads, how deep each branch is, and who is on which
	@scripts/pbt.py forks $(ENCLAVE)

proposals: ## Watching: who was due to propose each slot, and who missed
	@scripts/pbt.py proposals $(ENCLAVE)

diagnose: ## Watching: where did the chain split, and what were the peers doing then
	@scripts/pbt.py diagnose $(ENCLAVE)

split: ## Chaos by hand: partition the network: majority | last participant (el and cl)
	@scripts/pbt.py split $(ENCLAVE)

heal: ## Chaos by hand: remove every partition and shaping rule
	@scripts/pbt.py heal $(ENCLAVE)

repeer: ## Chaos by hand: restart any consensus client that banned a peer or has none (lighthouse bans partitioned peers), wait for the mesh
	@scripts/pbt.py repeer $(ENCLAVE)

chaos-status: ## Chaos by hand: what pbtchaos is doing now, what is queued, and recent results
	@scripts/pbt.py chaos-status $(ENCLAVE)

scenario: ## Chaos by hand: run one reorg scenario (NAME=code-shared DEPTH=20 [MINORITY=3])
	@scripts/pbt.py scenario $(ENCLAVE) $(NAME) $(DEPTH) $(MINORITY)

# Reads --el name=url straight from the running enclave (same el- prefix and
# `kurtosis port print` lookup scripts/pbt.py itself uses), so this needs no new
# plumbing in that script. LOGS_DIR and BINARY_TRIE_TIME have no sane default —
# LOGS_DIR is where kurtosis logs are dumped and BINARY_TRIE_TIME is a per-run
# value printed by main.star as "migration fork: binaryTrieTime=..." — so both
# are required explicitly rather than guessed. `make migration` runs this itself.
judge: bin ## Judging: judge a migration run from its logs (LOGS_DIR=dir BINARY_TRIE_TIME=unix; --el/--pins from ENCLAVE/PINS)
	@test -n "$(LOGS_DIR)" || { echo "LOGS_DIR is required (where to dump the kurtosis logs)"; exit 2; }
	@test -n "$(BINARY_TRIE_TIME)" || { echo "BINARY_TRIE_TIME is required (main.star prints it as 'migration fork: binaryTrieTime=...')"; exit 2; }
	@mkdir -p $(LOGS_DIR)
	@# Same evidence the lap dumps: client logs corroborate heals, the two JSONL streams
	@# are the record. kurtosis prefixes every line with "[service] "; strip it.
	@for svc in $$(cd scripts && python3 -c "import pbt; print(' '.join(pbt.services('$(ENCLAVE)','el-') + pbt.services('$(ENCLAVE)','cl-')))"); do \
	  kurtosis service logs $(ENCLAVE) $$svc -a 2>/dev/null | sed 's/^\[[^]]*\] //' > $(LOGS_DIR)/$$svc.log || true; done
	@for svc in migration-monitor migration-chaos; do \
	  kurtosis service logs $(ENCLAVE) $$svc -a 2>/dev/null | sed 's/^\[[^]]*\] //' > $(LOGS_DIR)/$$svc.jsonl || true; done
	@els="$$(cd scripts && python3 -c "import pbt; print(' '.join('--el ' + n + '=' + pbt.url('$(ENCLAVE)', n, 'rpc') for n in pbt.services('$(ENCLAVE)', 'el-')))")"; \
	bin/verify-migration $$els \
	  --monitor-jsonl $(LOGS_DIR)/migration-monitor.jsonl \
	  --chaos-jsonl $(LOGS_DIR)/migration-chaos.jsonl \
	  --logs-dir $(LOGS_DIR) \
	  --pins $(PINS) \
	  --binary-trie-time $(BINARY_TRIE_TIME)

build: sources ## Building: every image ARGS needs (besu, two Gradle stages, only when a participant runs it)
	scripts/build-images.sh $(ARGS)

sources: ## Building: clone the forks this builds from, if they are not already beside this repo
	@scripts/sources.sh

bin: ## Building: every command as a host binary into bin/
	@mkdir -p bin
	go build -o bin/ ./cmd/...
	@echo "==> $$(ls bin | tr '\n' ' ')"

genesis: ## Building: regenerate genesis/genesis.json and print its root (for single-client debugging)
	@mkdir -p bin
	go build -o bin/gengenesis ./cmd/gengenesis
	./bin/gengenesis --out genesis/genesis.json --gaslimit 200000000

check:
	@command -v docker >/dev/null 2>&1 || { \
	  echo "docker not found. Install Docker Desktop, or OrbStack."; exit 1; }
	@docker info >/dev/null 2>&1 || { \
	  echo "docker is installed but not responding — start Docker and retry."; exit 1; }
	@command -v kurtosis >/dev/null 2>&1 || { \
	  echo "kurtosis not found: brew install kurtosis-tech/tap/kurtosis-cli"; exit 1; }
	@echo "==> docker and kurtosis are both present"
