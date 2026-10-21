# Shared transfer primitives. Source from the repository root after setting
# tmp, bin, model, cat_dir, origin, pub, and REG as needed.

holds() { jq -r --arg n "$1" '.nodes[] | select(.name == $n) | .holds // [] | .[]' "$model/result.json"; }
want() { # want NODE TARGET DELIVERY FIELD -> model value
  jq -r --arg n "$1" --arg t "$2" --arg d "$3" \
    ".matrix[] | select(.node == \$n and .target == \$t and .delivery == \$d) | $4" "$model/result.json"
}

served() { curl -sf "$origin/metrics" | awk '$1 == "k3snix_nar_bytes_served" { print $2 }'; }
ncopy() { # ncopy STORE-ROOT LABEL
  nix copy --from "$origin" --to "$1" --option substituters "" \
    --option trusted-public-keys "$pub" \
    --option narinfo-cache-positive-ttl 0 --option narinfo-cache-negative-ttl 0 \
    "$(jq -r '.root' "$cat_dir/$2.json")"
}
valid() { # valid STORE-ROOT -> sorted valid paths, none if the store is empty
  [ -d "$1/nix/store" ] || return 0
  nix path-info --store "$1" --all 2>/dev/null | sort
}

layer_info() { # layer_info G LABEL -> "diffID blobDigest wireSize rawSize" per layer
  jq -r --slurpfile m "$tmp/$1-$2.manifest" --slurpfile i "$model/images/$1/$2.json" '
    [.rootfs.diff_ids, $m[0].layers, $i[0].layers] | transpose[]
    | if .[0] != .[2].diffID then error("diffID order differs") else . end
    | "\(.[0]) \(.[1].digest) \(.[1].size) \(.[2].size)"' "$tmp/$1-$2.config"
}

reg_lines() { docker logs "$REG" 2>&1 | wc -l; }
# blob_gets FROM-LINE -> "digest written" for blob GETs logged after FROM-LINE
blob_gets() {
  docker logs "$REG" 2>&1 | tail -n +"$(($1 + 1))" |
    grep 'response completed' | grep 'http.request.method=GET' | grep '/blobs/sha256:' |
    sed -E 's#.*/blobs/(sha256:[0-9a-f]+).*http\.response\.written=([0-9]+).*#\1 \2#'
}
# settle: wait until the registry log stops growing (it is written after
# the response).
settle() {
  local a b
  a=$(reg_lines)
  for _ in $(seq 1 20); do sleep 0.5; b=$(reg_lines); [ "$a" = "$b" ] && return; a=$b; done
}

skopeo_() { HOME="$tmp/skopeo-home" skopeo --policy "$tmp/policy.json" "$@"; }
