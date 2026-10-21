#!/usr/bin/env bash
# hack/demo-real-closures.sh — the Phase 1 data plane on the real pinned
# closures, without a cluster. It prints:
#
#   1. the churn ladder from summary.json: closure-size delta versus the
#      bytes a node on the previous version actually transfers
#   2. a missing_bytes matrix: every catalog closure against three simulated
#      nodes (holding churn-v1, holding hello, empty), answered by real agents
#   3. NAR parity: every path of three closures served by an agent with no
#      nix-store on PATH (so the pure-Go NAR writer runs), hashed and compared
#      with the narHash nix recorded in the manifest
#   4. a stock nix client copying the hello closure from a signed,
#      catalog-only agent into an empty store
#
# Simulated node stores hold empty entries named like the store paths: the
# inventory only lists names, so missing_bytes needs nothing more. NAR
# serving and the copy use the host /nix/store, which `nix build .#closures`
# populates. Nothing is fetched on a pod start; there are no pods.
set -euo pipefail

cd "$(dirname "$0")/.."

PORT_BASE="${K3SNIX_DEMO_PORT:-19860}"

fail() { echo "FAIL: $*" >&2; exit 1; }

for tool in nix jq curl; do
  command -v "$tool" >/dev/null 2>&1 || fail "missing tool: $tool (run inside 'nix develop')"
done

tmp="$(mktemp -d)"
pids=()
cleanup() {
  for pid in "${pids[@]}"; do kill "$pid" >/dev/null 2>&1 || true; done
  chmod -R u+w "$tmp" 2>/dev/null || true
  rm -rf "$tmp"
}
trap cleanup EXIT

echo "== nix build .#k3snix .#closures"
agent="$(nix build .#k3snix --no-link --print-out-paths)/bin/k3snix-agent"
closures="$(nix build .#closures --no-link --print-out-paths)"
cat_dir="$closures/manifests"
labels=$(cd "$cat_dir" && ls -- *.json | sed 's/\.json$//')

# start NAME PORT ARGS... — run an agent in the background with an empty
# PATH, so it never shells out to nix-store, and wait until it answers.
start() {
  local name="$1" port="$2"
  shift 2
  env PATH= "$agent" -listen "127.0.0.1:$port" "$@" >"$tmp/$name.log" 2>&1 &
  pids+=($!)
  curl -sf --retry 20 --retry-connrefused --retry-delay 1 -o /dev/null "http://127.0.0.1:$port/healthz" ||
    fail "agent $name did not start: $(cat "$tmp/$name.log")"
}

# --- 1. churn ladder --------------------------------------------------------

echo
echo "## 1. churn ladder (summary.json, nixpkgs $(jq -r '.nixpkgsRev[:12]' "$closures/summary.json"))"
jq -r '
  ["step", "closure delta", "delta %", "transfer bytes", "transfer %"],
  (.churn.steps[], .churn.fromV1[1:][]
    | ["\(.from) -> \(.to)", .delta, .ratioPct, .transferBytes, .transferPct])
  | @tsv' "$closures/summary.json" | column -t -s $'\t'
jq -r '"shared by hello and coreutils: \(.sharing.shared.names | join(", ")); unrelated pair shares \(.sharing.unrelatedShared.count) paths"' \
  "$closures/summary.json"

# --- 2. missing_bytes matrix -------------------------------------------------

mknode() { # mknode NAME LABEL... — a store holding exactly those closures
  local dir="$tmp/$1"
  shift
  mkdir -p "$dir"
  for label in "$@"; do
    jq -r '.paths[].path' "$cat_dir/$label.json" | while read -r p; do mkdir -p "$dir/$(basename "$p")"; done
  done
}
nodes=(node-churn-v1 node-hello node-empty)
mknode node-churn-v1 churn-v1
mknode node-hello share-a
mknode node-empty
declare -A url
port=$PORT_BASE
for n in "${nodes[@]}"; do
  start "$n" "$port" -store "$tmp/$n" -catalog "$cat_dir"
  url[$n]="http://127.0.0.1:$port"
  port=$((port + 1))
done

echo
echo "## 2. missing_bytes: bytes still to transfer / closure total"
{
  printf 'closure'
  printf '\t%s' "${nodes[@]}"
  printf '\n'
  for label in $labels; do
    printf '%s' "$label"
    for n in "${nodes[@]}"; do
      resp=$(curl -sf -XPOST "${url[$n]}/v1/missing_bytes" -d "{\"label\":\"$label\"}") ||
        fail "missing_bytes $label on $n"
      printf '\t%s' "$(jq -r '"\(.bytes)/\(.total_bytes) (\(.bytes * 1000 / .total_bytes | round / 10)%)"' <<<"$resp")"
    done
    printf '\n'
  done
} | column -t -s $'\t'

# --- 3. NAR parity -----------------------------------------------------------

real_port=$port
start real "$real_port" -store /nix/store -catalog "$cat_dir"

echo
echo "## 3. NAR parity: pure-Go NARs from the agent vs nix's narHash"
ok=0
bytes=0
for label in churn-v4 share-b unrelated-a; do
  while IFS=$'\t' read -r p want size; do
    hash=$(basename "$p" | cut -c1-32)
    curl -sf "http://127.0.0.1:$real_port/nar/$hash.nar" -o "$tmp/x.nar" || fail "NAR $p not served"
    got="sha256:$(nix hash file --type sha256 --base32 "$tmp/x.nar" 2>/dev/null)"
    [ "$got" = "$want" ] || fail "$p: NAR $got, manifest narHash $want"
    [ "$(wc -c <"$tmp/x.nar")" -eq "$size" ] || fail "$p: NAR size differs from narSize $size"
    ok=$((ok + 1))
    bytes=$((bytes + size))
  done < <(jq -r '.paths[] | [.path, .narHash, .narSize] | @tsv' "$cat_dir/$label.json")
done
echo "  $ok NARs ($bytes bytes) byte-identical to nix's narHash and narSize"

# --- 4. stock nix client -----------------------------------------------------

echo
echo "## 4. nix copy from a signed, catalog-only agent into an empty store"
nix key generate-secret --key-name k3snix-demo-1 >"$tmp/key"
pub=$(nix key convert-secret-to-public <"$tmp/key")
cache_port=$((real_port + 1))
start cache "$cache_port" -store /nix/store -catalog "$cat_dir" -catalog-only -sign-key-file "$tmp/key"
hello=$(jq -r '.root' "$cat_dir/share-a.json")
# A fresh key on a reused URL: bypass nix's narinfo cache.
nix copy --from "http://127.0.0.1:$cache_port" --to "$tmp/root" \
  --option trusted-public-keys "$pub" --option narinfo-cache-positive-ttl 0 "$hello" ||
  fail "nix copy of $hello"
echo "  copied $(ls "$tmp/root/nix/store" | wc -l) paths; $("$tmp/root$hello/bin/hello" --version | head -n 1)"
code=$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:$cache_port/$(basename "$closures" | cut -c1-32).narinfo")
[ "$code" = "404" ] || fail "catalog-only agent served a path outside the catalog ($code)"
echo "  a host path outside the catalog: $code"

echo
echo "## agent metrics (NAR parity agent)"
curl -sf "http://127.0.0.1:$real_port/metrics"
