#!/usr/bin/env bash
# Real stock K3s + Docker placement versus the Phase 2 rule on live agents.
# Run with nix develop -c bash hack/k3s-docker-live.sh.
set -euo pipefail
cd "$(dirname "$0")/.."
source hack/lib/compare-live.sh
fail() { echo "FAIL: $*" >&2; exit 1; }
RUNS=${K3SNIX_K3SD_RUNS:-6}
[[ "$RUNS" =~ ^[1-9][0-9]*$ ]] && ((RUNS % 3 == 0)) || fail 'K3SNIX_K3SD_RUNS must be a positive multiple of 3'
for tool in nix jq curl docker skopeo kubectl python3; do
  command -v "$tool" >/dev/null || fail "missing $tool; run inside nix develop"
done
components=$(docker version --format '{{range .Server.Components}}{{.Name}} {{end}}')
[[ "$components" == *Engine* && "$components" != *Podman* ]] || fail "not Docker Engine: $components"
echo "== host: Docker Engine $(docker version --format '{{.Server.Version}}')"
tmp=$(mktemp -d)
PREFIX="k3snix-k3sd-${tmp##*/}"
NET="$PREFIX-net" REG="$PREFIX-registry"
AGENT_PORT=${K3SNIX_K3SD_AGENT_PORT:-19880}
REGISTRY_PORT=${K3SNIX_K3SD_REGISTRY_PORT:-19890}
pids=() containers=() network_created=false
cleanup() {
  local rc=$?
  trap - EXIT
  for pid in "${pids[@]}"; do kill "$pid" 2>/dev/null || true; done
  for c in "${containers[@]}"; do
    if [ "$rc" -ne 0 ]; then docker logs --tail 15 "$c" >&2 2>/dev/null || true; docker exec "$c" tail -40 /tmp/k3s.log >&2 2>/dev/null || true; fi
    docker rm -f -v "$c" >/dev/null 2>&1 || true
  done
  if "$network_created"; then docker network rm "$NET" >/dev/null 2>&1 || true; fi
  chmod -R u+w "$tmp" 2>/dev/null || true
  rm -rf "$tmp"
  exit "$rc"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
bin="$(nix build .#k3snix --no-link --print-out-paths)/bin"
closures=$(nix build .#closures --no-link --print-out-paths)
model=$(nix build .#comparison --no-link --print-out-paths)
k3s_static=$(nix build .#k3s-static --no-link --print-out-paths)
expected_version=$(k3s --version | head -1 | awk '{print $3}')
actual_version=$("$k3s_static" --version | head -1 | awk '{print $3}')
[ "$expected_version" = "$actual_version" ] || fail "static $actual_version != flake $expected_version"
echo "== K3s $actual_version; $RUNS repetitions per target and arm"
cat_dir="$closures/manifests"
labels=$(jq -r '[.matrix[].target] | unique | .[]' "$model/result.json")
nodes=$(jq -r '.nodes[].name' "$model/result.json")
results="$tmp/results.tsv"
printf 'arm\ttarget\trun\tmode\tworker\tnode\tfirst_registered\tties\tmodel_raw\tlive_raw\tlive_wire\n' >"$results"

# Nix is independent of Docker; a fresh store and inventory on every query.
export XDG_CACHE_HOME="$tmp/xdg-cache"
nix key generate-secret --key-name k3snix-k3sd-1 >"$tmp/key"
pub=$(nix key convert-secret-to-public <"$tmp/key")
env PATH= "$bin/k3snix-agent" -listen "127.0.0.1:$AGENT_PORT" -store /nix/store \
  -catalog "$cat_dir" -catalog-only -sign-key-file "$tmp/key" >"$tmp/origin.log" 2>&1 &
pids+=("$!")
origin="http://127.0.0.1:$AGENT_PORT"
curl -sf --retry 20 --retry-connrefused --retry-delay 1 "$origin/healthz" >/dev/null
for t in $labels; do
  for r in $(seq 1 "$RUNS"); do
    : >"$tmp/scores"
    node_pids=() i=0
    for n in $nodes; do
      root="$tmp/stores/$n"
      mkdir -p "$root/nix/store"
      [ -z "$(valid "$root")" ] || fail "fresh store not empty: $n"
      for h in $(holds "$n"); do ncopy "$root" "$h" >/dev/null 2>&1; done
      seeded=$(for h in $(holds "$n"); do jq -r '.paths[].path' "$cat_dir/$h.json"; done | sort -u)
      [ "$(valid "$root")" = "$seeded" ] || fail "Nix seed mismatch: $n"
      port=$((AGENT_PORT + 1 + i)) i=$((i + 1))
      env PATH= "$bin/k3snix-agent" -listen "127.0.0.1:$port" -store "$root/nix/store" \
        -ttl 0s >"$tmp/$n.log" 2>&1 &
      node_pids+=("$!") pids+=("$!")
      curl -sf --retry 20 --retry-connrefused --retry-delay 1 "http://127.0.0.1:$port/healthz" >/dev/null
      missing=$(curl -sf -H 'Content-Type: application/json' --data-binary "@$cat_dir/$t.json" \
        "http://127.0.0.1:$port/v1/missing_bytes" | jq -er '.bytes')
      [ "$missing" = "$(want "$n" "$t" nix .raw.missing)" ] || fail "agent/model mismatch: $n/$t"
      printf '%s\t%s\n' "$missing" "$n" >>"$tmp/scores"
    done
    minimum=$(sort -n "$tmp/scores" | head -1 | cut -f1)
    ties=$(awk -v m="$minimum" '$1 == m {print $2}' "$tmp/scores" | sort | paste -sd,)
    chosen=${ties%%,*} # deterministic tie break, ties remain in the result
    b0=$(served)
    ncopy "$tmp/stores/$chosen" "$t" >/dev/null 2>&1
    got=$(($(served) - b0))
    [ "$got" = "$minimum" ] || fail "Nix origin sent $got, expected $minimum"
    printf 'k3snix\t%s\t%s\targmin\t-\t%s\t-\t%s\t%s\t%s\t%s\n' "$t" "$r" "$chosen" "$ties" "$minimum" "$got" "$got" >>"$results"
    for pid in "${node_pids[@]}"; do kill "$pid"; wait "$pid" 2>/dev/null || true; done
    # Remove reaped PIDs so cleanup cannot signal a later process reusing one.
    pids=("${pids[0]}")
    chmod -R u+w "$tmp/stores"
    rm -rf "$tmp/stores"
  done
  echo "  k3snix $t: $RUNS live decisions match the model"
done

# Registry is private; workloads never enter the host Docker image store.
docker network create "$NET" >/dev/null
network_created=true
containers+=("$REG")
docker run -d --name "$REG" --network "$NET" -p "127.0.0.1:$REGISTRY_PORT:5000" \
  docker.io/library/registry:2 >/dev/null
for _ in $(seq 1 60); do curl -sf "http://127.0.0.1:$REGISTRY_PORT/v2/" >/dev/null && break; sleep 1; done
curl -sf "http://127.0.0.1:$REGISTRY_PORT/v2/" >/dev/null || fail 'registry not ready'
mkdir -p "$tmp/skopeo-home"
echo '{"default":[{"type":"insecureAcceptAnything"}]}' >"$tmp/policy.json"
for g in fine coarse; do
  for t in $labels; do
    archive=$(nix build ".#ociImages.$g.$t" --no-link --print-out-paths)
    skopeo_ copy -q --dest-tls-verify=false "docker-archive:$archive" "docker://127.0.0.1:$REGISTRY_PORT/k3snix-oci/$t:$g"
    skopeo_ inspect --raw --tls-verify=false "docker://127.0.0.1:$REGISTRY_PORT/k3snix-oci/$t:$g" >"$tmp/$g-$t.manifest"
    skopeo_ inspect --config --tls-verify=false "docker://127.0.0.1:$REGISTRY_PORT/k3snix-oci/$t:$g" >"$tmp/$g-$t.config"
  done
done

# All Docker mutations below address our nested daemons only.
workers=(node-worker-1 node-worker-2 node-worker-3)
states=(node-churn-v1 node-hello node-empty)
node_container() { echo "$PREFIX-$g-$1"; }
nd() { local n=$1; shift; docker exec "$(node_container "$n")" docker "$@"; }
k() { docker exec -i "$(node_container node-control)" /usr/local/bin/k3s kubectl "$@"; }
image_ref() { echo "$REG:5000/k3snix-oci/$1:$g"; }
image_state() {
  local ids
  ids=$(nd "$1" image ls -aq --no-trunc | sort -u)
  if [ -n "$ids" ]; then
    nd "$1" image inspect $ids | jq -S '[.[] | {Id, layers: .RootFS.Layers}] | sort_by(.Id)'
  else echo '[]'; fi
}
state_of() { jq -er --arg n "$1" '.[$n]' "$tmp/assignment.json"; }

# Docker's containerd image store has runtime-specific size accounting. Probe
# identical registry pulls in a disposable daemon, never in a measured worker.
g=probe
probe=$(node_container sizes)
containers+=("$probe")
docker run -d --privileged --name "$probe" --network "$NET" -e DOCKER_TLS_CERTDIR= \
  docker.io/library/docker:29-dind --insecure-registry "$REG:5000" >/dev/null
for _ in $(seq 1 60); do docker exec "$probe" docker info >/dev/null 2>&1 && break; sleep 1; done
echo '{}' >"$tmp/runtime-sizes.json"
for g in fine coarse; do
  for t in $labels; do
    docker exec "$probe" docker pull -q "$(image_ref "$t")" >/dev/null
    size=$(docker exec "$probe" docker image inspect "$(image_ref "$t")" --format '{{.Size}}')
    jq --arg g "$g" --arg t "$t" --argjson size "$size" '.[$g][$t] = $size' \
      "$tmp/runtime-sizes.json" >"$tmp/sizes.next"
    mv "$tmp/sizes.next" "$tmp/runtime-sizes.json"
  done
done
docker rm -f -v "$probe" >/dev/null
containers=("$REG")
"$bin/k3snix-bench" compare -manifests "$cat_dir" -images "$model/images" \
  -nar-gzip "$model/nar-gzip.json" -runtime-image-sizes "$tmp/runtime-sizes.json" \
  -image-locality-nodes 4 -json "$tmp/runtime-model.json" >"$tmp/runtime-model.md"
jq -e 'all(.placement[] | select(.arm | startswith("vanilla")); all(.scores[]; . == 0))' \
  "$tmp/runtime-model.json" >/dev/null || fail 'runtime-sized ImageLocality model is not tied'

cluster_status() {
  k get nodes -o json >"$tmp/status.json"
  jq -e '.items | length == 4 and all(.[];
    any(.status.conditions[]; .type == "Ready" and .status == "True") and
    (.status.nodeInfo.containerRuntimeVersion | startswith("docker://")) and
    (.spec.unschedulable != true) and
    (if .metadata.name == "node-control" then
      any(.spec.taints[]?; .key == "node-role.kubernetes.io/control-plane" and .effect == "NoSchedule")
    else ((.spec.taints // []) | length == 0) end))' "$tmp/status.json" >/dev/null
}
verify_order() {
  local order
  order=$(jq -r '.items | sort_by(.metadata.creationTimestamp) | .[].metadata.name' "$tmp/status.json")
  [ "$order" = "$(printf '%s\n' node-control "${workers[@]}")" ] || fail "unexpected registration order: $order"
  jq -e '[.items[].metadata.creationTimestamp] | length == (unique | length)' "$tmp/status.json" >/dev/null || fail 'ambiguous registration timestamps'
}
# Three consecutive matches of both Docker IDs/diffIDs and kubelet images.
seed_status() {
  local n h actual expected stable=0 ok
  expected=$(for n in "${workers[@]}"; do
    for h in $(holds "$(state_of "$n")"); do printf '%s\t%s\n' "$n" "$(image_ref "$h")"; done
  done | sort)
  for _ in $(seq 1 90); do
    cluster_status || fail 'node runtime/readiness/taint check failed'
    verify_order
    actual=$(jq -r --arg reg "$REG:5000/" '.items[] | .metadata.name as $n | .status.images[]?.names[] | select(startswith($reg) and (contains("@") | not)) | "\($n)\t\(.)"' "$tmp/status.json" | sort)
    ok=true
    for n in node-control "${workers[@]}"; do
      [ "$(image_state "$n")" = "$(cat "$tmp/$n.seed.json")" ] || ok=false
    done
    if "$ok" && [ "$actual" = "$expected" ]; then
      stable=$((stable + 1))
      [ "$stable" -ge 3 ] && return
    else stable=0
    fi
    sleep 1
  done
  fail 'Docker/Node.status inventories did not return to seed state for three snapshots'
}
check_sizes() {
  # Includes the tainted control plane: ImageLocality uses the full snapshot.
  jq -e --arg g "$g" --arg reg "$REG:5000/k3snix-oci/" --slurpfile sizes "$tmp/runtime-sizes.json" '
    .items as $nodes | [$nodes[].status.images[]? | . as $image |
      .names[] | select(startswith($reg) and endswith(":" + $g)) |
      {name:., size:$image.sizeBytes}] as $images |
    all($images[]; . as $i | (.name | ltrimstr($reg) | split(":")[0]) as $label |
      .size == $sizes[0][$g][$label] and
      (.size * ([$images[] | select(.name == $i.name)] | length) / ($nodes | length)) < (23 * 1024 * 1024))
  ' "$tmp/status.json" >/dev/null || fail 'CRI size differs from model or exceeds ImageLocality floor'
}
start_cluster() {
  local n c role
  for n in node-control "${workers[@]}"; do
    c=$(node_container "$n")
    containers+=("$c")
    docker run -d --privileged --name "$c" --hostname "$n" --network "$NET" \
      --mount "type=bind,src=$k3s_static,dst=/usr/local/bin/k3s,readonly" \
      -e DOCKER_TLS_CERTDIR= docker.io/library/docker:29-dind \
      --insecure-registry "$REG:5000" >/dev/null
    for _ in $(seq 1 60); do nd "$n" info >/dev/null 2>&1 && break; sleep 1; done
    nd "$n" info >/dev/null || fail "dockerd not ready: $n"
    role=agent
    args=(--server "https://$(node_container node-control):6443")
    if [ "$n" = node-control ]; then
      role=server
      args=(--disable traefik,servicelb,metrics-server,local-storage,coredns --disable-network-policy \
        --node-taint node-role.kubernetes.io/control-plane:NoSchedule)
    fi
    docker exec -d "$c" sh -c 'echo $$ >/tmp/k3s.pid; exec /usr/local/bin/k3s "$@" >/tmp/k3s.log 2>&1' sh \
      "$role" --docker --token k3snix-k3sd-test-token --node-name "$n" \
      --pause-image docker.io/rancher/mirrored-pause:3.10.2 --kubelet-arg fail-swap-on=false \
      --kubelet-arg node-status-update-frequency=1s "${args[@]}"
    # Serialize registration and separate timestamps (Kubernetes uses seconds).
    ready=false
    for _ in $(seq 1 180); do
      if k get node "$n" -o json >"$tmp/node.json" 2>/dev/null && \
        jq -e 'any(.status.conditions[]?; .type == "Ready" and .status == "True")' "$tmp/node.json" >/dev/null; then ready=true; break; fi
      sleep 1
    done
    "$ready" || fail "node did not become ready: $n"
    sleep 2
    nd "$n" pull -q docker.io/rancher/mirrored-pause:3.10.2 >/dev/null
    image_state "$n" >"$tmp/$n.infra.json"
    infra=$(jq -r '.[].layers[]' "$tmp/$n.infra.json" | sort -u)
    workload=$(jq -r '.layers[].diffID' "$model"/images/*/*.json | sort -u)
    [ -z "$(comm -12 <(echo "$infra") <(echo "$workload"))" ] || fail "workload shares infrastructure: $n"
  done
  cluster_status || fail 'runtime/taint check failed'
  verify_order
  jq -e '[.items[] | select(.metadata.name != "node-control") | .status.allocatable | {cpu, memory}] | unique | length == 1' "$tmp/status.json" >/dev/null || fail 'unequal worker CPU/memory'
  k get nodes -o wide
  : >"$tmp/nodes-$g.jsonl"
}
seed_rotation() {
  local i n h ids
  jq -n --argjson r "$r" '{"node-worker-1": ["node-churn-v1","node-hello","node-empty"][(($r-1)%3)],
    "node-worker-2": ["node-churn-v1","node-hello","node-empty"][($r%3)],
    "node-worker-3": ["node-churn-v1","node-hello","node-empty"][((($r+1)%3))]}' >"$tmp/assignment.json"
  for n in node-control "${workers[@]}"; do
    ids=$(nd "$n" image ls --format '{{.Repository}}:{{.Tag}}' | grep -F "$REG:5000/" || true)
    [ -z "$ids" ] || nd "$n" image rm $ids >/dev/null
    [ "$(image_state "$n")" = "$(cat "$tmp/$n.infra.json")" ] || fail "rotation did not restore infrastructure: $n"
    if [ "$n" != node-control ]; then
      for h in $(holds "$(state_of "$n")"); do nd "$n" pull -q "$(image_ref "$h")" >/dev/null; done
    fi
    image_state "$n" >"$tmp/$n.seed.json"
  done
  first=$(state_of "${workers[0]}")
  seed_status
  check_sizes
  jq --argjson run "$r" --slurpfile assignment "$tmp/assignment.json" --arg first "$first" '
    {run:$run, firstRegisteredState:$first, assignment:$assignment[0],
     registrationOrder:(.items | sort_by(.metadata.creationTimestamp) | map(.metadata.name)),
     nodes:[.items[] | {name:.metadata.name, creationTimestamp:.metadata.creationTimestamp,
       runtime:.status.nodeInfo.containerRuntimeVersion, kubelet:.status.nodeInfo.kubeletVersion,
       allocatable:.status.allocatable, taints:(.spec.taints // []), images:.status.images,
       ready:any(.status.conditions[]; .type == "Ready" and .status == "True"),
       unschedulable:(.spec.unschedulable // false)}]}' "$tmp/status.json" >>"$tmp/nodes-$g.jsonl"
  echo "  rotation $r: first worker holds $first"
}
measure() {
  local mode=$1 pinned=$2 chosen pod pod_uid cfg have raw wire expected got stopped stable ids state
  seed_status
  check_sizes
  settle
  from=$(reg_lines)
  pod="k3snix-k3sd-$t-$r-$mode-${pinned:-free}"
  pod_uid=$(jq -n --arg p "$pod" --arg img "$(image_ref "$t")" --arg n "$pinned" '
    {apiVersion:"v1",kind:"Pod",metadata:{name:$p},spec:{schedulerName:"default-scheduler",restartPolicy:"Never",terminationGracePeriodSeconds:0,
      containers:[{name:"app",image:$img,imagePullPolicy:"IfNotPresent"}]}} |
    if $n != "" then .spec.nodeName = $n else . end' | k create -f - -o jsonpath='{.metadata.uid}')
  chosen=''
  for _ in $(seq 1 120); do
    chosen=$(k get pod "$pod" -o jsonpath='{.spec.nodeName}')
    if [ -n "$chosen" ] && nd "$chosen" image inspect "$(image_ref "$t")" >/dev/null 2>&1; then break; fi
    sleep 1
  done
  [[ " ${workers[*]} " == *" $chosen "* && -n "$chosen" ]] || fail "pod not assigned a measured worker: $pod/$chosen"
  [ -z "$pinned" ] || [ "$chosen" = "$pinned" ] || fail 'nodeName was not respected'
  nd "$chosen" image inspect "$(image_ref "$t")" >/dev/null || fail "pod image not pulled: $pod"
  state=$(state_of "$chosen")
  settle
  cfg=$(jq -r '.config.digest' "$tmp/$g-$t.manifest")
  blob_gets "$from" | grep -v "^$cfg " >"$tmp/got" || true
  have=$(jq -r '.[].layers[]' "$tmp/$chosen.seed.json" | sort -u)
  raw=0 wire=0
  : >"$tmp/expected"
  while read -r diff blob size tar; do
    grep -qxF "$diff" <<<"$have" && continue
    echo "$blob" >>"$tmp/expected"
    raw=$((raw + tar)) wire=$((wire + size))
  done < <(layer_info "$g" "$t")
  expected=$(jq -r --arg t "$t" --arg a "vanilla ($g)" --arg n "$state" '.placement[] | select(.target == $t and .arm == $a) | .transferByNode[$n]' "$tmp/runtime-model.json")
  [ "$raw" = "$expected" ] || fail "whole-image model mismatch: $g/$t/$state: $raw != $expected"
  [ "$(sort "$tmp/expected")" = "$(awk '{print $1}' "$tmp/got" | sort)" ] || fail "unexpected registry blobs: $g/$t/$state"
  got=$(awk '{s += $2} END {print s+0}' "$tmp/got")
  [ "$got" = "$wire" ] || fail "registry bytes $got != $wire"
  # Check each target's Docker accounting; the seeded images' CRI sizes are
  # independently checked by check_sizes before every scheduling decision.
  actual_size=$(nd "$chosen" image inspect "$(image_ref "$t")" --format '{{.Size}}')
  [ "$actual_size" = "$(jq -r --arg g "$g" --arg t "$t" '.[$g][$t]' "$tmp/runtime-sizes.json")" ] || fail "Docker size differs from probe: $g/$t/$chosen"
  printf 'vanilla-%s\t%s\t%s\t%s\t%s\t%s\t%s\t-\t%s\t%s\t%s\n' "$g" "$t" "$r" "$mode" "$chosen" "$state" "$first" "$expected" "$raw" "$got" >>"$results"
  k delete pod "$pod" --wait=true --timeout=60s >/dev/null
  stopped=false
  for _ in $(seq 1 60); do
    if k get --raw "/api/v1/nodes/$chosen/proxy/pods" >"$tmp/kubelet-pods.json" 2>/dev/null &&
      jq -e --arg uid "$pod_uid" 'all((.items // [])[]; .metadata.uid != $uid)' "$tmp/kubelet-pods.json" >/dev/null; then stopped=true; break; fi
    sleep 1
  done
  "$stopped" || fail "kubelet still desires deleted Pod $pod"
  stable=0
  for _ in $(seq 1 60); do
    ids=$(nd "$chosen" ps -aq --filter "label=io.kubernetes.pod.uid=$pod_uid")
    [ -z "$ids" ] || nd "$chosen" rm -f $ids >/dev/null 2>&1 || true
    if ! holds "$state" | grep -qxF "$t"; then nd "$chosen" image rm "$(image_ref "$t")" >/dev/null 2>&1 || true; fi
    if [ "$(image_state "$chosen")" = "$(cat "$tmp/$chosen.seed.json")" ]; then
      stable=$((stable + 1))
      [ "$stable" -ge 3 ] && break
    else stable=0
    fi
    sleep 1
  done
  [ "$stable" -ge 3 ] || fail "cannot restore seeded image IDs/diffIDs: $chosen/$t"
  seed_status
  echo "  $g $t $r $mode: $chosen ($state), $raw raw / $got wire bytes"
}
for g in fine coarse; do
  echo "== stock K3s + Docker: $g"
  start_cluster
  for r in $(seq 1 "$RUNS"); do
    seed_rotation
    for t in $labels; do
      measure free ''
      # Costs are deterministic by have-set: pin once per target/state/style.
      if [ "$r" = 1 ]; then
        for n in "${workers[@]}"; do measure pinned "$n"; done
      fi
    done
  done
  for n in node-control "${workers[@]}"; do docker rm -f -v "$(node_container "$n")" >/dev/null; done
  containers=("$REG")
done
cat "$results"
python3 hack/summarize-k3s-docker.py "$results" | tee "$tmp/summary.md"
out=${K3SNIX_K3SD_OUT:-docs/evidence/k3s-docker-live.tsv}
mkdir -p "$(dirname "$out")"
cp "$results" "$out"
jq -n --arg k3s "$actual_version" --arg hostDocker "$(docker version --format '{{.Server.Version}}')" \
  --arg measuredAt "$(date -u -Iseconds)" --arg model "$model" --arg commit "$(git rev-parse HEAD)" \
  --arg status "$(git status --porcelain)" --arg workDiff "$(git diff --binary | sha256sum | cut -d' ' -f1)" \
  --arg indexDiff "$(git diff --cached --binary | sha256sum | cut -d' ' -f1)" \
  --arg scriptHash "$(sha256sum hack/k3s-docker-live.sh | cut -d' ' -f1)" --argjson runs "$RUNS" \
  '{measuredAt:$measuredAt,k3s:$k3s,hostDocker:$hostDocker,model:$model,commit:$commit,
    gitStatusPorcelain:$status,unstagedDiffSHA256:$workDiff,stagedDiffSHA256:$indexDiff,
    scriptSHA256:$scriptHash,runs:$runs,pinnedRuns:1,totalNodes:4,
    rotation:"seed assignment rotates across workers in verified creationTimestamp order",
    imageSizeSource:"Docker image inspect .Size, cross-checked against Node.status.images.sizeBytes"}' >"$out.metadata.json"
cp "$tmp/summary.md" "$out.summary.md"
cp "$tmp/runtime-sizes.json" "$out.image-sizes.json"
cp "$tmp/runtime-model.json" "$out.model.json"
for g in fine coarse; do
  jq -s '{source:"kubectl get nodes -o json; seed snapshots used for floor check",runs:.}' \
    "$tmp/nodes-$g.jsonl" >"$out.nodes-$g.json"
done
echo '== k3s-docker-live passed'
