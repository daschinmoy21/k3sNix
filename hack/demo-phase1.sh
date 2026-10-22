#!/usr/bin/env bash
# hack/demo-phase1.sh — deploy an app, watch every node's agent report what
# it is missing, and collect the data. Opens a live view
# (hack/demo/index.html) that animates the requests and replies.
#
# Three real k3snix-agent processes run over simulated node stores built from
# the pinned closures: node-warm holds churn-v1, node-mid holds hello
# (share-a), node-cold is empty. As in hack/demo-real-closures.sh, a
# simulated store holds empty entries named like the store paths, which is
# all the inventory reads. Every number shown comes from an agent's
# /v1/missing_bytes answer.
#
# Knobs:
#   DEMO_APP=churn-v4  closure label to deploy (any label in the catalog)
#   DEMO_PACE=1        multiplier on every pause (0 runs straight through)
#   DEMO_VIZ=0         no browser view
#   DEMO_VIZ_PORT      browser view port (default 19880)
#   K3SNIX_DEMO_PORT   first agent port (default 19860)
set -euo pipefail

cd "$(dirname "$0")/.."

APP="${DEMO_APP:-churn-v4}"
PACE="${DEMO_PACE:-1}"
VIZ="${DEMO_VIZ:-1}"
VIZ_PORT="${DEMO_VIZ_PORT:-19880}"
PORT_BASE="${K3SNIX_DEMO_PORT:-19860}"

fail() { echo "FAIL: $*" >&2; exit 1; }

tools=(nix jq curl awk)
[ "$VIZ" = 1 ] && tools+=(python3)
for tool in "${tools[@]}"; do
  command -v "$tool" >/dev/null 2>&1 || fail "missing tool: $tool (run inside 'nix develop')"
done

tmp="$(mktemp -d)"
pids=()
cleanup() {
  for pid in "${pids[@]}"; do kill "$pid" >/dev/null 2>&1 || true; done
  rm -rf "$tmp"
}
trap cleanup EXIT

nap() { sleep "$(awk -v a="$1" -v p="$PACE" 'BEGIN { print a * p }')"; }

human() {
  awk -v b="$1" 'BEGIN {
    if (b >= 1e6) printf "%.1f MB", b / 1e6
    else if (b >= 1e3) printf "%.0f KB", b / 1e3
    else printf "%d B", b }'
}

web="$tmp/web"
events="$web/events.jsonl"
mkdir -p "$web"
: >"$events"

# emit TYPE [jq --arg/--argjson pairs...] — append one event for the view.
emit() {
  local type="$1"
  shift
  jq -nc --arg type "$type" "$@" '$ARGS.named' >>"$events"
}

echo "== nix build .#k3snix .#closures"
agent="$(nix build .#k3snix --no-link --print-out-paths)/bin/k3snix-agent"
closures="$(nix build .#closures --no-link --print-out-paths)"
cat_dir="$closures/manifests"
[ -f "$cat_dir/$APP.json" ] || fail "no closure labeled $APP in $cat_dir"

if [ "$VIZ" = 1 ]; then
  cp hack/demo/index.html "$web/index.html"
  python3 -m http.server --bind 127.0.0.1 --directory "$web" "$VIZ_PORT" >"$tmp/viz.log" 2>&1 &
  pids+=($!)
  curl -sf --retry 20 --retry-connrefused --retry-delay 1 -o /dev/null "http://127.0.0.1:$VIZ_PORT/" ||
    fail "view did not start: $(cat "$tmp/viz.log")"
  echo "== live view: http://127.0.0.1:$VIZ_PORT/"
  if [ -n "${DISPLAY:-}${WAYLAND_DISPLAY:-}" ] && command -v xdg-open >/dev/null 2>&1; then
    xdg-open "http://127.0.0.1:$VIZ_PORT/" >/dev/null 2>&1 || true
  fi
  if [ -t 0 ]; then read -r -p "   press Enter to deploy " _; else nap 3; fi
fi

# --- cluster -----------------------------------------------------------------

nodes=(node-warm node-mid node-cold)
declare -A holds=([node-warm]=churn-v1 [node-mid]=share-a [node-cold]="")
declare -A url
port=$PORT_BASE
for n in "${nodes[@]}"; do
  mkdir -p "$tmp/$n"
  for label in ${holds[$n]}; do
    jq -r '.paths[].path' "$cat_dir/$label.json" | while read -r p; do mkdir -p "$tmp/$n/$(basename "$p")"; done
  done
  env PATH= "$agent" -listen "127.0.0.1:$port" -store "$tmp/$n" -catalog "$cat_dir" >"$tmp/$n.log" 2>&1 &
  pids+=($!)
  url[$n]="http://127.0.0.1:$port"
  curl -sf --retry 20 --retry-connrefused --retry-delay 1 -o /dev/null "${url[$n]}/healthz" ||
    fail "agent on $n did not start: $(cat "$tmp/$n.log")"
  emit node --arg node "$n" --arg holds "${holds[$n]:-nothing}" \
    --argjson paths "$(find "$tmp/$n" -mindepth 1 -maxdepth 1 | wc -l)"
  port=$((port + 1))
done
nap 1.5

# --- deploy --------------------------------------------------------------------

id=$(jq -r '.paths | sort_by(.path)[] | "\(.path) \(.narSize)"' "$cat_dir/$APP.json" | sha256sum | cut -c1-32)
total=$(jq '[.paths[].narSize] | add' "$cat_dir/$APP.json")
count=$(jq '.paths | length' "$cat_dir/$APP.json")
echo
echo "== deploy $APP (closure $id, $count store paths, $(human "$total"))"
emit deploy --arg app "$APP" --arg id "$id" --argjson paths "$count" --argjson bytes "$total"
nap 2

printf '   %-10s %-10s %-22s %-12s %s\n' node holds missing "have paths" "answered in"
for n in "${nodes[@]}"; do
  emit ask --arg node "$n"
  nap 1
  r=$(curl -sf -XPOST "${url[$n]}/v1/missing_bytes" -d "{\"id\":\"$id\"}") || fail "missing_bytes on $n"
  emit report --arg node "$n" --argjson r "$r"
  jq -r --arg n "$n" --arg h "${holds[$n]:--}" \
    '[$n, $h, "\(.bytes) B (\(.bytes * 1000 / .total_bytes | round / 10)%)", "\(.have_paths)/\(.total_paths)", "\(.compute_ms) ms"] | @tsv' <<<"$r" |
    while IFS=$'\t' read -r a b c d e; do printf '   %-10s %-10s %-22s %-12s %s\n' "$a" "$b" "$c" "$d" "$e"; done
  nap 1.5
done
emit "done"

echo
echo "== done"
if [ "$VIZ" = 1 ]; then
  if [ -t 0 ]; then read -r -p "   press Enter to stop " _; else nap 10; fi
fi
