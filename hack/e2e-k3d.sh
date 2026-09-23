#!/usr/bin/env bash
# hack/e2e-k3d.sh — k3d smoke test for the k3snix data plane.
#
# Seeding strategy:
#   k3d does not share the host nix store with agent emptyDirs: every agent
#   pod gets a fresh emptyDir at /data/store, so the fixture closure is
#   seeded into it at startup, by node name (-seed-by-node -seed-label=e2e):
#     * node name ending in server-0 (or containing "warm") -> warm: every path
#     * node name containing agent-0 (or "mid")             -> mid: first half
#     * anything else                                       -> cold: no paths
#   The /nix/store volume (--volume /nix/store:/nix/store@all) exists so the
#   origin pod can serve the store paths that `nix build .#closures` already
#   realized on the host; the agents themselves never see it. Cold holds
#   none of the fixture. Nothing fetches on pod start: the agent only ever
#   serves bytes a client asks for.
#
# Fallback: if the cluster cannot be created with the /nix/store volume, the
# script runs the built k3snix-agent on the host at 0.0.0.0:9877 with
# -store /nix/store, skips deploy/20-origin.yaml, and points the DaemonSet
# origin URL at http://host.k3d.internal:9877 instead. The assertion that a
# real churn-v1 path's narinfo returns 200 still runs, against the host
# agent; it is never dropped.
#
# Catalog: the smoke creates the catalog ConfigMap from the fixture before
# applying the manifests (this is the single documented command for it):
#   kubectl -n k3snix create configmap k3snix-catalog --from-file=e2e.json=testdata/e2e/e2e.json
set -euo pipefail

cd "$(dirname "$0")/.."

CLUSTER="${K3SNIX_CLUSTER_NAME:-k3snix}"
KEEP="${K3SNIX_E2E_KEEP:-0}"
IMAGE_TAR="${K3SNIX_IMAGE_TAR:-}"
CLOSURES_DIR="${K3SNIX_CLOSURES_DIR:-}"

FIXTURE="testdata/e2e/e2e.json"
PEER_HASH="aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

fail() { echo "FAIL: $*" >&2; exit 1; }

for tool in docker k3d kubectl jq curl python3 nix; do
  command -v "$tool" >/dev/null 2>&1 || fail "missing tool: $tool (run inside 'nix develop')"
done

tmp="$(mktemp -d)"
host_agent_pid=""
pf_pid=""

cleanup() {
  if [ -n "$pf_pid" ]; then kill "$pf_pid" >/dev/null 2>&1 || true; fi
  if [ -n "$host_agent_pid" ]; then kill "$host_agent_pid" >/dev/null 2>&1 || true; fi
  if [ "$KEEP" != "1" ]; then
    echo "== deleting k3d cluster $CLUSTER"
    k3d cluster delete "$CLUSTER" >/dev/null 2>&1 || true
  fi
  rm -rf "$tmp"
}
trap cleanup EXIT

# --- build inputs ---------------------------------------------------------

if [ -z "$IMAGE_TAR" ]; then
  echo "== nix build .#image"
  nix build .#image -o "$tmp/result-image"
  IMAGE_TAR="$tmp/result-image"
fi
if [ -z "$CLOSURES_DIR" ]; then
  echo "== nix build .#closures"
  nix build .#closures -o "$tmp/result-closures"
  CLOSURES_DIR="$tmp/result-closures"
fi
[ -f "$CLOSURES_DIR/manifests/churn-v1.json" ] || fail "no manifests/churn-v1.json under $CLOSURES_DIR"

echo "== docker load $IMAGE_TAR (tag k3snix:dev)"
docker load -i "$IMAGE_TAR"

# --- cluster ---------------------------------------------------------------

if k3d cluster get "$CLUSTER" >/dev/null 2>&1; then
  fail "cluster $CLUSTER already exists; delete it or set K3SNIX_CLUSTER_NAME"
fi

export KUBECONFIG="$tmp/kubeconfig" # k3d merges the new cluster here only

echo "== k3d cluster create $CLUSTER (agents 2, /nix/store volume on all nodes)"
FALLBACK=0
if ! k3d cluster create "$CLUSTER" --agents 2 --volume /nix/store:/nix/store@all \
    >"$tmp/k3d-create.log" 2>&1; then
  echo "== cluster create with the /nix/store volume failed; falling back to a host origin agent"
  tail -n 20 "$tmp/k3d-create.log" >&2 || true
  FALLBACK=1
  k3d cluster delete "$CLUSTER" >/dev/null 2>&1 || true
  if ! k3d cluster create "$CLUSTER" --agents 2 >>"$tmp/k3d-create.log" 2>&1; then
    tail -n 20 "$tmp/k3d-create.log" >&2 || true
    fail "cannot create k3d cluster $CLUSTER"
  fi
fi

if [ "$FALLBACK" = 1 ]; then
  echo "== nix build .#k3snix (host origin agent)"
  nix build .#k3snix -o "$tmp/result-bin"
  "$tmp/result-bin/bin/k3snix-agent" -listen 0.0.0.0:9877 -store /nix/store \
    >"$tmp/host-agent.log" 2>&1 &
  host_agent_pid=$!
fi

echo "== importing image k3snix:dev into $CLUSTER"
k3d image import k3snix:dev -c "$CLUSTER"

echo "== waiting for 3 Ready nodes"
ready=0
for _ in $(seq 1 60); do
  total=$(kubectl get nodes --no-headers 2>/dev/null | wc -l)
  up=$(kubectl get nodes --no-headers 2>/dev/null | awk '$2 ~ /^Ready/' | wc -l)
  if [ "$total" -eq 3 ] && [ "$up" -eq 3 ]; then ready=1; break; fi
  sleep 2
done
if [ "$ready" != "1" ]; then
  kubectl get nodes || true
  fail "did not reach 3 Ready nodes"
fi

# Cluster DNS must be up before agents can resolve the origin service.
kubectl -n kube-system rollout status deployment/coredns --timeout=120s

# --- deploy ----------------------------------------------------------------

echo "== applying manifests"
kubectl apply -f deploy/00-namespace.yaml
kubectl -n k3snix create configmap k3snix-catalog --from-file=e2e.json="$FIXTURE"
if [ "$FALLBACK" = 1 ]; then
  sed 's#http://k3snix-origin.k3snix.svc.cluster.local:9877#http://host.k3d.internal:9877#' \
    deploy/10-agent-daemonset.yaml >"$tmp/agent-ds.yaml"
  kubectl apply -f "$tmp/agent-ds.yaml"
else
  kubectl apply -f deploy/
fi

if ! kubectl -n k3snix rollout status daemonset/k3snix-agent --timeout=180s; then
  kubectl -n k3snix get pods -o wide || true
  kubectl -n k3snix describe daemonset/k3snix-agent || true
  fail "daemonset/k3snix-agent did not become ready"
fi
if [ "$FALLBACK" = 0 ]; then
  if ! kubectl -n k3snix rollout status deployment/k3snix-origin --timeout=180s; then
    kubectl -n k3snix get pods -o wide || true
    kubectl -n k3snix logs deployment/k3snix-origin --all-containers || true
    fail "deployment/k3snix-origin did not become ready"
  fi
fi

# --- helpers ---------------------------------------------------------------

# probe POD URL [BODY] — one HTTP request from inside POD via the agent's
# one-shot client mode; prints the response body to stdout.
probe() {
  local pod="$1" url="$2" body="${3:-}"
  if [ -n "$body" ]; then
    kubectl -n k3snix exec "$pod" -- /bin/k3snix-agent -probe-url="$url" -probe-body="$body"
  else
    kubectl -n k3snix exec "$pod" -- /bin/k3snix-agent -probe-url="$url"
  fi
}

pod_role() {
  kubectl -n k3snix logs "$1" | grep -o 'role=[a-z]*' | head -n 1 | cut -d= -f2
}

pod_with_role() {
  local want="$1" pod role
  for pod in $pods; do
    role=$(pod_role "$pod")
    if [ "$role" = "$want" ]; then
      echo "$pod"
      return 0
    fi
  done
  fail "no agent pod with role $want"
}

metric() { # metric POD NAME
  probe "$1" http://127.0.0.1:9860/metrics | awk -v m="$2" '$1 == m { print $2 }'
}

nixbase32() { # hex sha -> nix base32 (bit groups read LSB-first per byte,
  # like nix and internal/nixbase32 — NOT big-endian)
  python3 - "$1" <<'PY'
import sys
alphabet = "0123456789abcdfghijklmnpqrsvwxyz"
b = bytes.fromhex(sys.argv[1])
nchars = (len(b) * 8 + 4) // 5
out = []
for i in range(nchars - 1, -1, -1):
    bit = i * 5
    byte, shift = divmod(bit, 8)
    v = b[byte] >> shift
    if byte + 1 < len(b):
        v |= b[byte + 1] << (8 - shift)
    out.append(alphabet[v & 0x1F])
print("".join(out))
PY
}

pods=$(kubectl -n k3snix get pods -l app=k3snix-agent -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}')

# --- seeded missing-bytes ---------------------------------------------------

echo "== missing_bytes per role"
seen_warm=0
seen_mid=0
seen_cold=0
for pod in $pods; do
  node=$(kubectl -n k3snix get pod "$pod" -o jsonpath='{.spec.nodeName}')
  role=$(pod_role "$pod")
  [ -n "$role" ] || fail "$pod ($node): no seed role in logs"
  resp=$(probe "$pod" http://127.0.0.1:9860/v1/missing_bytes '{"label":"e2e"}')
  bytes=$(jq -r '.bytes' <<<"$resp")
  case "$role" in
    warm) want=0 ;;
    mid) want=120 ;;
    cold) want=150 ;;
    *) fail "$pod ($node): unknown role '$role'" ;;
  esac
  [ "$bytes" = "$want" ] || fail "$pod ($node, role $role): missing bytes $bytes, want $want"
  echo "  $node role=$role missing_bytes=$bytes"
  case "$role" in
    warm) seen_warm=1 ;;
    mid) seen_mid=1 ;;
    cold) seen_cold=1 ;;
  esac
done
if [ "$seen_warm" != 1 ] || [ "$seen_mid" != 1 ] || [ "$seen_cold" != 1 ]; then
  fail "expected one agent pod of each role warm/mid/cold"
fi

# --- peer NAR (mid pod fetches a real NAR from the warm pod) ----------------

echo "== peer NAR fetch from mid pod via warm pod"
warm_pod=$(pod_with_role warm)
mid_pod=$(pod_with_role mid)
warm_ip=$(kubectl -n k3snix get pod "$warm_pod" -o jsonpath='{.status.podIP}')
[ -n "$warm_ip" ] || fail "no pod IP for $warm_pod"

narinfo=$(probe "$mid_pod" "http://${warm_ip}:9860/${PEER_HASH}.narinfo")
[ -n "$narinfo" ] || fail "empty narinfo for ${PEER_HASH}"
narurl=$(awk '/^URL: /{print $2}' <<<"$narinfo")
[ -n "$narurl" ] || fail "narinfo has no URL field: $narinfo"

probe "$mid_pod" "http://${warm_ip}:9860/${narurl}" >"$tmp/peer.nar"
actual_size=$(wc -c <"$tmp/peer.nar" | tr -d ' ')
actual_hash=$(sha256sum "$tmp/peer.nar" | cut -d' ' -f1)
want_hash=$(awk '/^NarHash: /{print $2}' <<<"$narinfo")
want_size=$(awk '/^NarSize: /{print $2}' <<<"$narinfo")

[ "$want_hash" = "sha256:$(nixbase32 "$actual_hash")" ] ||
  fail "NarHash mismatch: narinfo $want_hash, NAR sha256:$(nixbase32 "$actual_hash")"
[ "$actual_size" = "$want_size" ] ||
  fail "NarSize mismatch: NAR has $actual_size bytes, narinfo says $want_size"
echo "  NarHash $want_hash verified over $actual_size bytes"

# --- origin counters --------------------------------------------------------

echo "== origin counters on a node agent"
cold_pod=$(pod_with_role cold)
req0=$(metric "$cold_pod" k3snix_origin_requests)
[ "$req0" = "0" ] || fail "k3snix_origin_requests starts at $req0, want 0"

# The first probe may race the origin service's DNS/iptables settling, so
# retry until the upstream GET succeeds; failed probes do not move counters.
probe_ok=0
for _ in $(seq 1 10); do
  if probe "$cold_pod" http://127.0.0.1:9860/v1/origin_probe '{}' >/dev/null 2>&1; then
    probe_ok=1
    break
  fi
  sleep 3
done
[ "$probe_ok" = "1" ] || fail "POST /v1/origin_probe never succeeded"

req=$(metric "$cold_pod" k3snix_origin_requests)
obytes=$(metric "$cold_pod" k3snix_origin_bytes)
[ "$req" -ge 1 ] || fail "k3snix_origin_requests $req after probe, want >= 1"
[ "$obytes" -gt 0 ] || fail "k3snix_origin_bytes $obytes after probe, want > 0"
echo "  k3snix_origin_requests=$req k3snix_origin_bytes=$obytes"

# --- origin preload ---------------------------------------------------------

echo "== origin preload for a real churn-v1 path"
churn_hash=$(jq -r '.paths[0].path' "$CLOSURES_DIR/manifests/churn-v1.json" | awk -F/ '{print $NF}' | cut -c1-32)
[ "${#churn_hash}" -eq 32 ] || fail "bad churn-v1 hash prefix '$churn_hash'"

if [ "$FALLBACK" = 0 ]; then
  kubectl -n k3snix port-forward svc/k3snix-origin 19877:9877 >/dev/null 2>&1 &
  pf_pid=$!
  origin_base="http://127.0.0.1:19877"
else
  origin_base="http://127.0.0.1:9877"
fi

get_code() { curl -sS --retry 20 --retry-connrefused --retry-delay 1 -o /dev/null -w '%{http_code}' "$1"; }

code=$(get_code "$origin_base/nix-cache-info")
[ "$code" = "200" ] || fail "$origin_base/nix-cache-info -> $code"
code=$(get_code "$origin_base/${churn_hash}.narinfo")
[ "$code" = "200" ] || fail "$origin_base/${churn_hash}.narinfo -> $code (churn-v1 path must be served)"
echo "  /nix-cache-info and /${churn_hash}.narinfo both 200"

if [ -n "$pf_pid" ]; then
  kill "$pf_pid" >/dev/null 2>&1 || true
  wait "$pf_pid" 2>/dev/null || true
  pf_pid=""
fi

echo "== e2e smoke passed (fallback=$FALLBACK)"
