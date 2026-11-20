#!/usr/bin/env bash
# hack/compare-live.sh — check the store-path versus layer model
# (nix build .#comparison) against real transfers by each ecosystem's stock
# client, on the same workloads and the same three simulated nodes:
#
#   nix   a stock `nix copy` pulls from a signed, catalog-only k3snix-agent
#         origin into a node store; bytes come from the agent's
#         k3snix_nar_bytes_served counter.
#   oci   a stock `docker pull` (Docker Engine, in a Docker-in-Docker node)
#         pulls from a registry:2 origin; bytes come from the registry's
#         access log (layer blob GETs).
#
# Every measurement starts from a fresh node: a new, empty Nix store
# directory, or a new dind daemon with an empty image store. The node is
# seeded with what it holds by the same client from the same origin, then
# the target is pulled and the bytes the origin sent for that pull alone are
# compared with the model. The two sides share no cache:
#   * the nix side never talks to Docker; its stores are fresh directories
#     and its only source is the agent (no substituters, a private
#     XDG_CACHE_HOME so nix's narinfo disk cache starts empty);
#   * the oci side never reads /nix/store at pull time and never uses the
#     host Docker image store for workloads: images go from their nix-built
#     archives to the registry with skopeo (private HOME, so its blob-info
#     cache starts empty), and every node is a new daemon that is deleted
#     with its volume afterwards.
# The script fails if `docker` is not Docker Engine (for example a podman
# shim), or if any measured transfer differs from the model.
set -euo pipefail

cd "$(dirname "$0")/.."

AGENT_PORT="${K3SNIX_CMP_AGENT_PORT:-19870}"
REGISTRY_PORT="${K3SNIX_CMP_REGISTRY_PORT:-19871}"
REGISTRY_IMAGE="docker.io/library/registry:2"
DIND_IMAGE="docker.io/library/docker:29-dind"

source hack/lib/compare-live.sh

fail() { echo "FAIL: $*" >&2; exit 1; }

for tool in nix jq curl docker skopeo; do
  command -v "$tool" >/dev/null 2>&1 || fail "missing tool: $tool (run inside 'nix develop')"
done
components=$(docker version --format '{{range .Server.Components}}{{.Name}} {{end}}') ||
  fail "docker daemon not reachable"
case " $components" in
  *Podman*) fail "docker is Podman ($components); the OCI side must run on Docker Engine" ;;
  *" Engine "*) ;;
  *) fail "docker server is not Docker Engine (components: $components)" ;;
esac
echo "== docker: Docker Engine $(docker version --format '{{.Server.Version}}')"

tmp="$(mktemp -d)"
NET="k3snix-k3sd-transfer-${tmp##*/}"
REG="$NET-registry"
NODE="$NET-node"
agent_pid=""
cleanup() {
  [ -n "$agent_pid" ] && kill "$agent_pid" >/dev/null 2>&1 || true
  docker rm -f -v "$NODE" "$REG" >/dev/null 2>&1 || true
  docker network rm "$NET" >/dev/null 2>&1 || true
  chmod -R u+w "$tmp" 2>/dev/null || true
  rm -rf "$tmp"
}
trap cleanup EXIT

echo "== nix build .#k3snix .#closures .#comparison"
bin="$(nix build .#k3snix --no-link --print-out-paths)/bin"
closures="$(nix build .#closures --no-link --print-out-paths)"
model="$(nix build .#comparison --no-link --print-out-paths)"
cat_dir="$closures/manifests"
labels=$(jq -r '[.matrix[].target] | unique | .[]' "$model/result.json")
nodes=$(jq -r '.nodes[].name' "$model/result.json")

results="$tmp/results.tsv"
printf 'delivery\tnode\ttarget\tmodel content raw\tlive content raw\tlive whole-image wire\tcontent units\n' >"$results"

# --- nix side ----------------------------------------------------------------

echo "== nix side: signed catalog-only origin, fresh store per measurement"
export XDG_CACHE_HOME="$tmp/xdg-cache"
nix key generate-secret --key-name k3snix-cmp-1 >"$tmp/key"
pub=$(nix key convert-secret-to-public <"$tmp/key")
env PATH= "$bin/k3snix-agent" -listen "127.0.0.1:$AGENT_PORT" -store /nix/store \
  -catalog "$cat_dir" -catalog-only -sign-key-file "$tmp/key" >"$tmp/agent.log" 2>&1 &
agent_pid=$!
origin="http://127.0.0.1:$AGENT_PORT"
curl -sf --retry 20 --retry-connrefused --retry-delay 1 -o /dev/null "$origin/healthz" ||
  fail "origin agent did not start: $(cat "$tmp/agent.log")"

for n in $nodes; do
  for t in $labels; do
    root="$tmp/nix/$n-$t"
    mkdir -p "$root"
    [ -z "$(valid "$root")" ] || fail "nix $n/$t: fresh store is not empty"
    for h in $(holds "$n"); do ncopy "$root" "$h"; done
    seeded=$(valid "$root")
    want_seeded=$(for h in $(holds "$n"); do jq -r '.paths[].path' "$cat_dir/$h.json"; done | sort -u)
    [ "$seeded" = "$want_seeded" ] || fail "nix $n/$t: seeded store differs from what $n holds"
    b0=$(served)
    ncopy "$root" "$t"
    got=$(($(served) - b0))
    new=$(comm -13 <(echo "$seeded") <(valid "$root") | grep -c . || true)
    model_raw=$(want "$n" "$t" nix .raw.missing)
    model_units=$(want "$n" "$t" nix .units.missing)
    [ "$got" = "$model_raw" ] || fail "nix $n/$t: origin sent $got NAR bytes, model says $model_raw"
    [ "$new" = "$model_units" ] || fail "nix $n/$t: $new new paths, model says $model_units"
    printf 'nix\t%s\t%s\t%s\t%s\t%s\t%s\n' "$n" "$t" "$model_raw" "$got" "$got" "$new" >>"$results"
    chmod -R u+w "$root" && rm -rf "$root"
  done
done
echo "  $(($(wc -l <"$results") - 1)) nix transfers match the model"

# --- oci side ----------------------------------------------------------------

echo "== oci side: registry:2 origin, fresh Docker-in-Docker node per measurement"
docker rm -f -v "$NODE" "$REG" >/dev/null 2>&1 || true
docker network rm "$NET" >/dev/null 2>&1 || true
docker network create "$NET" >/dev/null
docker run -d --name "$REG" --network "$NET" -p "127.0.0.1:$REGISTRY_PORT:5000" "$REGISTRY_IMAGE" >/dev/null
# Poll: docker-proxy accepts and resets connections until the registry
# listens, and curl's --retry does not retry a reset.
for _ in $(seq 1 60); do
  curl -sf -o /dev/null "http://127.0.0.1:$REGISTRY_PORT/v2/" && break
  sleep 1
done
curl -sf -o /dev/null "http://127.0.0.1:$REGISTRY_PORT/v2/" || fail "registry did not start"

# skopeo gets a private HOME so its blob-info cache starts empty.
# A private policy accepts the local, unsigned test images.
mkdir -p "$tmp/skopeo-home"
echo '{"default":[{"type":"insecureAcceptAnything"}]}' >"$tmp/policy.json"

for g in fine coarse; do
  for t in $labels; do
    archive=$(nix build ".#ociImages.$g.$t" --no-link --print-out-paths)
    skopeo_ copy -q --dest-tls-verify=false "docker-archive:$archive" \
      "docker://127.0.0.1:$REGISTRY_PORT/k3snix-oci/$t:$g"
    # diffID -> blob digest and wire size, from the pushed manifest
    skopeo_ inspect --raw --tls-verify=false "docker://127.0.0.1:$REGISTRY_PORT/k3snix-oci/$t:$g" >"$tmp/$g-$t.manifest"
    skopeo_ inspect --config --tls-verify=false "docker://127.0.0.1:$REGISTRY_PORT/k3snix-oci/$t:$g" >"$tmp/$g-$t.config"
  done
done

dk() { docker exec "$NODE" docker "$@"; }
start_node() {
  docker rm -f -v "$NODE" >/dev/null 2>&1 || true
  docker run -d --privileged --name "$NODE" --network "$NET" -e DOCKER_TLS_CERTDIR= \
    "$DIND_IMAGE" --insecure-registry "$REG:5000" >/dev/null
  for _ in $(seq 1 60); do dk info >/dev/null 2>&1 && return 0; sleep 1; done
  docker logs "$NODE" | tail -20 >&2
  fail "dind node did not start"
}

overhead=$(jq -r '.overhead.diffID // ""' "$model/result.json")
for g in fine coarse; do
  d="oci-$g"
  for n in $nodes; do
    for t in $labels; do
      start_node
      [ -z "$(dk images -q)" ] || fail "oci $n/$t: fresh node is not empty"
      for h in $(holds "$n"); do dk pull -q "$REG:5000/k3snix-oci/$h:$g" >/dev/null; done
      settle
      from=$(reg_lines)
      dk pull -q "$REG:5000/k3snix-oci/$t:$g" >/dev/null
      settle

      # Layers the target needs that the seed images do not hold, by diffID.
      have=$(for h in $(holds "$n"); do layer_info "$g" "$h"; done | awk '{print $1}' | sort -u)
      layer_info "$g" "$t" >"$tmp/target"
      exp_raw=0 exp_wire=0 exp_units=0
      : >"$tmp/expected"
      while read -r diff blob wire raw; do
        grep -qxF "$diff" <<<"$have" && continue
        echo "$blob" >>"$tmp/expected"
        [ "$diff" = "$overhead" ] && continue # path-less layer: not in the model rows
        exp_raw=$((exp_raw + raw)) exp_units=$((exp_units + 1))
      done <"$tmp/target"
      while read -r diff blob wire raw; do
        grep -qxF "$blob" "$tmp/expected" && exp_wire=$((exp_wire + wire))
      done <"$tmp/target"

      model_raw=$(want "$n" "$t" "$d" .raw.missing)
      model_units=$(want "$n" "$t" "$d" .units.missing)
      [ "$exp_raw" = "$model_raw" ] || fail "oci $n/$t/$g: layer bytes missing $exp_raw, model says $model_raw"
      [ "$exp_units" = "$model_units" ] || fail "oci $n/$t/$g: $exp_units layers missing, model says $model_units"

      # What the registry actually sent for this pull: layer blobs only
      # (the config blob is metadata, like a narinfo).
      cfg=$(jq -r '.config.digest' "$tmp/$g-$t.manifest")
      blob_gets "$from" | grep -v "^$cfg " >"$tmp/got" || true
      got_blobs=$(awk '{print $1}' "$tmp/got" | sort)
      [ "$got_blobs" = "$(sort "$tmp/expected")" ] ||
        fail "oci $n/$t/$g: registry sent blobs [$got_blobs], expected [$(sort "$tmp/expected" | tr '\n' ' ')]"
      got_wire=$(awk '{s += $2} END {print s + 0}' "$tmp/got")
      [ "$got_wire" = "$exp_wire" ] || fail "oci $n/$t/$g: registry sent $got_wire bytes, manifest sizes say $exp_wire"
      # live raw: the uncompressed tar bytes of the content layers it sent
      printf '%s\t%s\t%s\t%s\t%s\t%s\t%s\n' "$d" "$n" "$t" "$model_raw" "$exp_raw" "$got_wire" "$exp_units" >>"$results"
    done
  done
done
docker rm -f -v "$NODE" >/dev/null 2>&1 || true
echo "  $(($(grep -c '^oci' "$results"))) docker pulls match the model"

echo
echo "## live transfers (raw/units = workload content only; wire = all payload layers, including path-less overhead)"
column -t -s $'\t' "$results"
if [ -n "${K3SNIX_CMP_OUT:-}" ]; then
  cp "$results" "$K3SNIX_CMP_OUT"
  echo "results written to $K3SNIX_CMP_OUT"
fi
echo
echo "== compare-live passed"
