#!/usr/bin/env bash
# hack/e2e-k3d.sh — k3d smoke test for the k3snix data plane.
#
# Seeding strategy:
#   k3d does not share the host nix store with the agents: every agent's
#   store is /var/lib/k3snix/store inside its own k3d node, so the fixture
#   closure is seeded into it at startup, by node name (-seed-by-node
#   -seed-label=e2e):
#     * node name ending in server-0 (or containing "warm") -> warm: every path
#     * node name ending in -agent-0 (or containing "mid")   -> mid: first half
#     * anything else                                       -> cold: no paths
#   The /nix/store volume (--volume /nix/store:/nix/store@all) exists so the
#   origin pod can serve the store paths that `nix build .#closures` already
#   realized on the host; the agents themselves never see it. Cold holds
#   none of the fixture. Nothing fetches on pod start: the agent only ever
#   serves bytes a client asks for.
#
# Origin: a k3snix-agent over the host store in -catalog-only mode, with the
# pinned closure manifests as its catalog (ConfigMap k3snix-origin-catalog),
# so it serves exactly the closures it is preloaded with. Its narinfo is
# signed with a key generated per run (Secret k3snix-origin-key), and the
# smoke has real nix verify the churn-v1 closure against the public key.
#
# Fallback: if the cluster cannot be created with the /nix/store volume, the
# script runs the built k3snix-agent on the host at 0.0.0.0:9877 with
# -store /nix/store and the same catalog and key, skips
# deploy/20-origin.yaml, and points the DaemonSet origin URL at
# http://host.k3d.internal:9877 instead. Every origin assertion still runs,
# against the host agent; none is dropped.
#
# Catalog: the smoke creates the catalog ConfigMap from the fixture before
# applying the manifests (this is the single documented command for it):
#   kubectl -n k3snix create configmap k3snix-catalog --from-file=e2e.json=testdata/e2e/e2e.json
# The smoke adds the pinned manifests to it, which /v1/fetch needs.
#
# Delta fetch: the cold agent fetches churn-v1 (all from the origin), the
# mid agent then fetches it again (all from the cold agent, a peer) and
# churn-v2 (only the delta), and two concurrent fetches of hello on the warm
# agent download each path once. Every fetched path must dump to the
# manifest narHash.
#
# Host store: hello and coreutils pods are placed by the k3snix scheduler,
# get their closure from an init container that calls /v1/fetch, and run
# from the node store mounted read-only at /nix/store. Deleting hello drops
# its GC root, and /v1/gc then removes hello but nothing coreutils needs.
#
# Scheduler: a second kube-scheduler (profile k3snix) calls the extender,
# whose catalog holds the fixture and the pinned manifests. A pod naming the
# fixture root must bind to the warm node, the extender's trace must explain
# why, and with the extender gone a k3snix pod must still be scheduled.
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

# The largest churn-v1 path: what the origin NAR probe and preload checks use.
churn_path=$(jq -r '.paths | max_by(.narSize) | .path' "$CLOSURES_DIR/manifests/churn-v1.json")
churn_size=$(jq -r '.paths | max_by(.narSize) | .narSize' "$CLOSURES_DIR/manifests/churn-v1.json")
churn_root=$(jq -r '.root' "$CLOSURES_DIR/manifests/churn-v1.json")
churn_hash=$(basename "$churn_path" | cut -c1-32)
[ "${#churn_hash}" -eq 32 ] || fail "bad churn-v1 hash prefix '$churn_hash'"

echo "== nix key generate-secret (origin narinfo signing key)"
nix key generate-secret --key-name k3snix-origin-1 >"$tmp/origin.key"
origin_pub=$(nix key convert-secret-to-public <"$tmp/origin.key")

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
    -catalog "$CLOSURES_DIR/manifests" -catalog-only -sign-key-file "$tmp/origin.key" \
    >"$tmp/host-agent.log" 2>&1 &
  host_agent_pid=$!
fi

echo "== importing image k3snix:dev into $CLUSTER"
k3d image import k3snix:dev -c "$CLUSTER"

# The second scheduler runs the upstream kube-scheduler of the server's
# version, pulled on the host and imported like the agent image.
k8s_version=$(kubectl version -o json | jq -r '.serverVersion.gitVersion' | sed 's/+.*//')
scheduler_image="registry.k8s.io/kube-scheduler:$k8s_version"
echo "== importing $scheduler_image into $CLUSTER"
docker pull -q "$scheduler_image" >/dev/null
k3d image import "$scheduler_image" -c "$CLUSTER"

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
kubectl -n k3snix create configmap k3snix-catalog \
  --from-file="$CLOSURES_DIR/manifests/" --from-file=e2e.json="$FIXTURE"
kubectl -n k3snix create configmap k3snix-origin-catalog --from-file="$CLOSURES_DIR/manifests/"
kubectl -n k3snix create secret generic k3snix-origin-key --from-file=secret-key="$tmp/origin.key"
kubectl -n k3snix create configmap k3snix-scheduler-catalog \
  --from-file="$CLOSURES_DIR/manifests/" --from-file=e2e.json="$FIXTURE"
mkdir "$tmp/deploy"
cp deploy/*.yaml "$tmp/deploy/"
sed -i "s#registry.k8s.io/kube-scheduler:v[0-9.]*#$scheduler_image#" "$tmp/deploy/31-scheduler.yaml"
if [ "$FALLBACK" = 1 ]; then
  sed -i 's#http://k3snix-origin.k3snix.svc.cluster.local:9877#http://host.k3d.internal:9877#' \
    "$tmp/deploy/10-agent-daemonset.yaml"
  rm "$tmp/deploy/20-origin.yaml"
fi
kubectl apply -f "$tmp/deploy/"

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
  local out
  out=$(kubectl -n k3snix logs "$1" 2>/dev/null | grep -o 'role=[a-z]*' | head -n 1) || true
  echo "${out#role=}"
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
  resp=$(probe "$pod" http://127.0.0.1:9860/v1/missing_bytes '{"label":"e2e"}') \
    || fail "$pod ($node): missing_bytes probe failed"
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

narinfo=$(probe "$mid_pod" "http://${warm_ip}:9860/${PEER_HASH}.narinfo") \
  || fail "narinfo probe failed for warm pod $warm_pod"
[ -n "$narinfo" ] || fail "empty narinfo for ${PEER_HASH}"
narurl=$(awk '/^URL: /{print $2}' <<<"$narinfo")
[ -n "$narurl" ] || fail "narinfo has no URL field: $narinfo"

probe "$mid_pod" "http://${warm_ip}:9860/${narurl}" >"$tmp/peer.nar" \
  || fail "NAR fetch failed from $warm_pod"
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
echo "  cache-info probe: k3snix_origin_requests=$req k3snix_origin_bytes=$obytes"

# A NAR probe moves real NAR bytes: the cold agent downloads the churn-v1
# path from the origin, verifies it against the narinfo and discards it.
resp=$(probe "$cold_pod" http://127.0.0.1:9860/v1/origin_probe "{\"hash\":\"$churn_hash\"}") \
  || fail "NAR origin probe for $churn_path failed"
[ "$(jq -r '.verified' <<<"$resp")" = "true" ] || fail "NAR origin probe not verified: $resp"
[ "$(jq -r '.nar_size' <<<"$resp")" = "$churn_size" ] ||
  fail "NAR origin probe nar_size $(jq -r '.nar_size' <<<"$resp"), manifest narSize $churn_size"
req2=$(metric "$cold_pod" k3snix_origin_requests)
obytes2=$(metric "$cold_pod" k3snix_origin_bytes)
[ "$req2" -eq $((req + 2)) ] || fail "k3snix_origin_requests $req2 after NAR probe, want $((req + 2))"
[ $((obytes2 - obytes)) -ge "$churn_size" ] ||
  fail "k3snix_origin_bytes grew by $((obytes2 - obytes)), want >= narSize $churn_size"
echo "  NAR probe: $(basename "$churn_path") verified, k3snix_origin_bytes=$obytes2 (+$((obytes2 - obytes)))"
[ "$(metric "$cold_pod" k3snix_store_paths)" = "0" ] || fail "NAR probe materialized paths on the cold node"

# --- origin preload ---------------------------------------------------------

echo "== origin preload for the pinned churn-v1 closure"

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

# Real nix checks signatures and NAR contents for the whole closure. The
# key is new every run but the origin URL is not, so nix's narinfo cache
# would replay the previous run's signatures; a zero TTL bypasses it.
nix store verify --store "$origin_base" --trusted-public-keys "$origin_pub" \
  --option narinfo-cache-positive-ttl 0 -r "$churn_root" \
  >"$tmp/verify.log" 2>&1 || { cat "$tmp/verify.log" >&2; fail "nix store verify of $churn_root against the origin"; }
echo "  nix store verify: churn-v1 closure signed by $(cut -d: -f1 <<<"$origin_pub") and content-checked"

# Preloaded means only the pinned closures: a host store path outside the
# catalog is not served even though the origin can see it.
# The closure-manifests output itself is such a path.
outside_path=$(readlink -f "$CLOSURES_DIR")
case "$outside_path" in /nix/store/*) ;; *) fail "closures dir '$outside_path' is not a store path" ;; esac
outside=$(basename "$outside_path" | cut -c1-32)
code=$(curl -sS -o /dev/null -w '%{http_code}' "$origin_base/${outside}.narinfo")
[ "$code" = "404" ] || fail "$origin_base/${outside}.narinfo -> $code, want 404 for a path outside the catalog"
echo "  /${outside}.narinfo ($(basename "$outside_path"), not in the catalog) 404"

if [ -n "$pf_pid" ]; then
  kill "$pf_pid" >/dev/null 2>&1 || true
  wait "$pf_pid" 2>/dev/null || true
  pf_pid=""
fi

# --- delta fetch -------------------------------------------------------------

echo "== delta fetch: peers first, origin second, verified before visible"

manifest_bytes() { jq '[.paths[].narSize] | add' "$CLOSURES_DIR/manifests/$1.json"; }
manifest_paths() { jq '.paths | length' "$CLOSURES_DIR/manifests/$1.json"; }

fetch() { # fetch POD LABEL — prints the /v1/fetch reply
  probe "$1" http://127.0.0.1:9860/v1/fetch "{\"label\":\"$2\"}"
}

# assert_fetched POD LABEL — every path of LABEL is in POD's store and the
# NAR it serves for each matches the manifest narHash.
assert_fetched() {
  local pod="$1" label="$2" resp p want h got ip
  resp=$(probe "$pod" http://127.0.0.1:9860/v1/missing_bytes "{\"label\":\"$label\"}")
  [ "$(jq -r '.bytes' <<<"$resp")" = "0" ] || fail "$pod still misses $label after fetch: $resp"
  ip=$(kubectl -n k3snix get pod "$pod" -o jsonpath='{.status.podIP}')
  while IFS=$'\t' read -r p want; do
    h=$(basename "$p" | cut -c1-32)
    probe "$pod" "http://$ip:9860/nar/$h.nar" >"$tmp/fetched.nar" || fail "$pod does not serve $p"
    got="sha256:$(nixbase32 "$(sha256sum "$tmp/fetched.nar" | cut -d' ' -f1)")"
    [ "$got" = "$want" ] || fail "$pod: $p dumps to $got, manifest narHash $want"
  done < <(jq -r '.paths[] | [.path, .narHash] | @tsv' "$CLOSURES_DIR/manifests/$label.json")
}

v1_bytes=$(manifest_bytes churn-v1)

# 1. The cold agent holds nothing of churn-v1, and neither do its peers.
origin0=$(metric "$cold_pod" k3snix_origin_bytes)
resp=$(fetch "$cold_pod" churn-v1) || fail "cold fetch of churn-v1"
[ "$(jq -r '.predicted_bytes' <<<"$resp")" = "$v1_bytes" ] || fail "cold predicted bytes: $resp"
[ "$(jq -r '.origin_bytes' <<<"$resp")" = "$v1_bytes" ] || fail "cold fetch did not come from the origin: $resp"
[ "$(jq -r '.peer_bytes' <<<"$resp")" = "0" ] || fail "cold fetch used a peer: $resp"
[ $(($(metric "$cold_pod" k3snix_origin_bytes) - origin0)) = "$v1_bytes" ] ||
  fail "k3snix_origin_bytes on cold did not grow by $v1_bytes"
assert_fetched "$cold_pod" churn-v1
echo "  cold: churn-v1 $v1_bytes bytes from the origin, every NAR matches its narHash"

# 2. The mid agent gets the same closure from the cold agent.
origin0=$(metric "$mid_pod" k3snix_origin_bytes)
resp=$(fetch "$mid_pod" churn-v1) || fail "mid fetch of churn-v1"
[ "$(jq -r '.peer_bytes' <<<"$resp")" = "$v1_bytes" ] || fail "mid fetch did not come from the peer: $resp"
[ "$(jq -r '.origin_bytes' <<<"$resp")" = "0" ] || fail "mid fetch used the origin: $resp"
[ "$(metric "$mid_pod" k3snix_origin_bytes)" = "$origin0" ] || fail "mid origin counter moved"
assert_fetched "$mid_pod" churn-v1
echo "  mid: churn-v1 $v1_bytes bytes from peer $(jq -r '.fetched[0].source' <<<"$resp" | cut -d' ' -f2), 0 from the origin"

# 3. Upgrading mid to churn-v2 moves only the delta.
resp=$(fetch "$mid_pod" churn-v2) || fail "mid fetch of churn-v2"
predicted=$(jq -r '.predicted_bytes' <<<"$resp")
fetched=$(jq -r '.fetched_bytes' <<<"$resp")
v2_bytes=$(manifest_bytes churn-v2)
[ "$predicted" = "$fetched" ] || fail "churn-v2 predicted $predicted, fetched $fetched"
[ "$fetched" -lt "$v2_bytes" ] || fail "churn-v2 fetched the whole closure ($fetched of $v2_bytes)"
[ "$(jq -r '.local_hits' <<<"$resp")" -gt 0 ] || fail "churn-v2 had no local hits: $resp"
assert_fetched "$mid_pod" churn-v2
echo "  mid: churn-v2 upgrade fetched $fetched of $v2_bytes bytes (predicted $predicted), $(jq -r '.local_hits' <<<"$resp") paths local"

# 4. Two concurrent fetches of one closure download every path once.
a_bytes=$(manifest_bytes share-a)
origin0=$(metric "$warm_pod" k3snix_origin_bytes)
fetch "$warm_pod" share-a >"$tmp/fetch-a1.json" &
f1=$!
fetch "$warm_pod" share-a >"$tmp/fetch-a2.json" &
f2=$!
wait "$f1" || fail "first concurrent fetch of share-a"
wait "$f2" || fail "second concurrent fetch of share-a"
together=$(( $(jq -r '.fetched_bytes' "$tmp/fetch-a1.json") + $(jq -r '.fetched_bytes' "$tmp/fetch-a2.json") ))
[ "$together" = "$a_bytes" ] || fail "concurrent fetches downloaded $together bytes together, closure is $a_bytes"
[ $(($(metric "$warm_pod" k3snix_origin_bytes) - origin0)) = "$a_bytes" ] ||
  fail "warm origin bytes grew by $(($(metric "$warm_pod" k3snix_origin_bytes) - origin0)), want $a_bytes"
assert_fetched "$warm_pod" share-a
echo "  warm: two concurrent share-a fetches moved $together bytes for a $a_bytes-byte closure"
for pod in "$cold_pod" "$mid_pod" "$warm_pod"; do
  [ "$(metric "$pod" k3snix_fetch_verify_failures)" = "0" ] || fail "$pod rejected a NAR"
done

# --- cache-aware scheduling ---------------------------------------------------

echo "== k3snix scheduler places a pod by missing bytes"
for d in k3snix-extender k3snix-scheduler; do
  if ! kubectl -n k3snix rollout status "deployment/$d" --timeout=180s; then
    kubectl -n k3snix get pods -o wide || true
    kubectl -n k3snix logs "deployment/$d" --all-containers --tail=50 || true
    fail "deployment/$d did not become ready"
  fi
done

fixture_root=$(jq -r '.root' "$FIXTURE")
warm_node=$(kubectl -n k3snix get pod "$warm_pod" -o jsonpath='{.spec.nodeName}')

# k3snix_pod NAME — a long-running pod for the k3snix scheduler that names
# the fixture closure by its root path. The agent image has no shell, so the
# agent itself (on its own image's store) keeps the container alive.
k3snix_pod() {
  kubectl apply -f - <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: $1
  namespace: default
  annotations:
    k3snix.io/root: $fixture_root
spec:
  schedulerName: k3snix
  containers:
    - name: idle
      image: k3snix:dev
      imagePullPolicy: Never
      command: ["/bin/k3snix-agent", "-listen=127.0.0.1:19999", "-store=/nix/store"]
EOF
}

# scheduled_node POD — waits until POD is bound and prints its node.
scheduled_node() {
  local node=""
  for _ in $(seq 1 60); do
    node=$(kubectl get pod "$1" -o jsonpath='{.spec.nodeName}' 2>/dev/null) || true
    [ -n "$node" ] && break
    sleep 2
  done
  [ -n "$node" ] || { kubectl describe pod "$1" >&2 || true; fail "pod $1 was never scheduled"; }
  echo "$node"
}

k3snix_pod k3snix-placed
placed=$(scheduled_node k3snix-placed)
trace=$(kubectl -n k3snix logs deployment/k3snix-extender | grep -F '"pod":"default/k3snix-placed"' | tail -n 1 | sed 's/.*prioritize //')
[ -n "$trace" ] || fail "no extender trace for default/k3snix-placed"
echo "  extender trace: $trace"
[ "$placed" = "$warm_node" ] || fail "k3snix-placed bound to $placed, want the warm node $warm_node"
warm_score=$(jq -r --arg n "$warm_node" '.nodes[] | select(.node == $n) | .score' <<<"$trace")
[ "$warm_score" = "10" ] || fail "warm node $warm_node scored $warm_score, want 10"
[ "$(jq '[.nodes[] | select(.score == 10)] | length' <<<"$trace")" = "1" ] ||
  fail "more than one node scored 10: $trace"
echo "  k3snix-placed -> $placed (warm, score 10; the other nodes miss bytes)"

# With the extender gone, the k3snix profile must still schedule pods with
# its default plugins (ignorable: true).
kubectl -n k3snix scale deployment/k3snix-extender --replicas=0
kubectl -n k3snix wait --for=delete pod -l app=k3snix-extender --timeout=60s
k3snix_pod k3snix-fallback
echo "  extender down: k3snix-fallback -> $(scheduled_node k3snix-fallback) (default scoring)"
kubectl delete pod k3snix-placed k3snix-fallback --wait=false >/dev/null
kubectl -n k3snix scale deployment/k3snix-extender --replicas=1

# --- host-store runtime -----------------------------------------------------

echo "== host-store pods: placed by k3snix, fetched by an init container, run read-only"
kubectl -n k3snix rollout status deployment/k3snix-extender --timeout=120s

# hoststore_pod NAME LABEL ARGV-JSON — the example workload for LABEL's root.
hoststore_pod() {
  local root
  root=$(jq -r '.root' "$CLOSURES_DIR/manifests/$2.json")
  sed -e "s#name: hoststore-app#name: $1#" \
    -e "s#k3snix.io/root: ROOT#k3snix.io/root: $root#" \
    -e "s#command: \[\"\$(ROOT)/bin/hello\"\]#command: $3#" \
    deploy/examples/hello-hoststore.yaml | kubectl apply -f -
}

# 1. hello: the warm node fetched share-a above, so it scores 10 there and
#    the init container finds every path local.
hoststore_pod hello share-a '["$(ROOT)/bin/hello"]'
kubectl wait --for=jsonpath='{.status.phase}'=Succeeded pod/hello --timeout=180s ||
  { kubectl describe pod hello >&2; fail "pod hello did not succeed"; }
[ "$(kubectl get pod hello -o jsonpath='{.spec.nodeName}')" = "$warm_node" ] || fail "hello not on the warm node"
out=$(kubectl logs hello -c app)
[ "$out" = "Hello, world!" ] || fail "hello printed '$out'"
init=$(kubectl logs hello -c k3snix-fetch)
[ "$(jq -r '.fetched_bytes' <<<"$init")" = "0" ] || fail "hello fetched bytes on a warm node: $init"
echo "  hello on $warm_node: '$out', init fetch moved 0 bytes ($(jq -r '.local_hits' <<<"$init") paths local)"

# 2. coreutils: the warm node holds most of it through hello's libraries.
want_missing=$(probe "$warm_pod" http://127.0.0.1:9860/v1/missing_bytes '{"label":"share-b"}' | jq -r '.bytes')
hoststore_pod tools share-b '["$(ROOT)/bin/sleep", "3600"]'
kubectl wait --for=condition=Ready pod/tools --timeout=180s ||
  { kubectl describe pod tools >&2; fail "pod tools did not become ready"; }
[ "$(kubectl get pod tools -o jsonpath='{.spec.nodeName}')" = "$warm_node" ] || fail "tools not on the warm node"
init=$(kubectl logs tools -c k3snix-fetch)
[ "$(jq -r '.predicted_bytes' <<<"$init")" = "$want_missing" ] || fail "tools predicted bytes: $init, missing_bytes said $want_missing"
[ "$(jq -r '.fetched_bytes' <<<"$init")" = "$want_missing" ] || fail "tools fetched more than the missing bytes: $init"
echo "  tools on $warm_node: fetched $want_missing of $(manifest_bytes share-b) bytes, the rest shared with hello"

tools_root=$(jq -r '.root' "$CLOSURES_DIR/manifests/share-b.json")
if kubectl exec tools -c app -- "$tools_root/bin/touch" /nix/store/k3snix-write-test 2>"$tmp/touch.err"; then
  fail "a pod wrote into the host store"
fi
grep -q 'Read-only file system' "$tmp/touch.err" || fail "touch failed for another reason: $(cat "$tmp/touch.err")"
echo "  writes to /nix/store from the pod fail: $(cat "$tmp/touch.err")"

# 3. GC roots: deleting hello drops its root; collecting then keeps every
#    path tools needs, the libraries hello shared with it included.
[ "$(metric "$warm_pod" k3snix_gc_roots)" = "2" ] || fail "warm agent holds $(metric "$warm_pod" k3snix_gc_roots) roots, want 2"
kubectl delete pod hello --wait=true >/dev/null
for _ in $(seq 1 30); do
  [ "$(metric "$warm_pod" k3snix_gc_roots)" = "1" ] && break
  sleep 2
done
[ "$(metric "$warm_pod" k3snix_gc_roots)" = "1" ] || fail "hello's root outlived the pod"
gc=$(probe "$warm_pod" http://127.0.0.1:9860/v1/gc '{}') || fail "gc on the warm agent"
hello_root=$(jq -r '.root' "$CLOSURES_DIR/manifests/share-a.json")
jq -e --arg p "$hello_root" '.removed | index($p)' <<<"$gc" >/dev/null || fail "gc kept hello: $gc"
resp=$(probe "$warm_pod" http://127.0.0.1:9860/v1/missing_bytes '{"label":"share-b"}')
[ "$(jq -r '.bytes' <<<"$resp")" = "0" ] || fail "gc removed paths tools needs: $resp"
kubectl exec tools -c app -- "$tools_root/bin/true" || fail "tools broke after gc"
echo "  gc removed $(jq '.removed | length' <<<"$gc") unrooted paths ($(jq -r '.removed_bytes' <<<"$gc") bytes), hello among them; tools still runs"
kubectl delete pod tools --wait=false >/dev/null

echo "== e2e smoke passed (fallback=$FALLBACK)"
