#!/usr/bin/env bash
# Print every execution client's head and state root side by side, so a divergence is
# visible at a glance rather than inferred from logs.
set -euo pipefail
ENCLAVE="${1:-pbt}"

# kurtosis port print includes the scheme only when the port declares an application
# protocol, so el rpc comes back as "127.0.0.1:1234" while disruptoor's http port comes
# back as "http://127.0.0.1:1234". Normalise rather than guess.
url() {
  local u
  u=$(kurtosis port print "$1" "$2" "$3" 2>/dev/null) || return 1
  [[ -z "$u" ]] && return 1
  case "$u" in http://*|https://*) printf '%s' "$u" ;; *) printf 'http://%s' "$u" ;; esac
}

els=$(kurtosis enclave inspect "$ENCLAVE" 2>/dev/null | awk '/^[0-9a-f]{12}/ {print $2}' | grep '^el-' || true)
[[ -n "$els" ]] || { echo "no execution clients in enclave '$ENCLAVE'" >&2; exit 1; }

printf '%-26s %8s  %s\n' CLIENT BLOCK "STATE ROOT"
roots=""
for el in $els; do
  url=$(url "$ENCLAVE" "$el" rpc) || continue
  read -r num root < <(curl -s -X POST "$url" -H 'Content-Type: application/json' \
    -d '{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["latest",false]}' \
    | python3 -c 'import json,sys
b=(json.load(sys.stdin).get("result") or {})
print(int(b.get("number","0x0"),16), b.get("stateRoot","?"))' 2>/dev/null || echo "0 ?")
  printf '%-26s %8s  %s\n' "$el" "$num" "$root"
  roots="$roots$root
"
done

# Heads legitimately differ by a block or two, so this compares roots only as a hint.
# scripts/compare-roots.sh is the real check: it compares the SAME block number.
uniq_roots=$(printf '%s' "$roots" | grep -v '^$' | sort -u | wc -l | tr -d ' ')
if [[ "$uniq_roots" == "1" ]]; then
  echo "all clients on the same root"
else
  echo "roots differ at the tip — normal if the heads differ; run 'make verify' to compare equal heights"
fi
