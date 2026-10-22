#!/usr/bin/env bash
# Moves the Dockerfile's Go build cache mount (/root/.cache/go-build) in and
# out of BuildKit, so actions/cache can persist it between runs. The type=gha
# layer cache does not export RUN --mount=type=cache contents. Same technique
# as reproducible-containers/buildkit-cache-dance, which the org's action
# allowlist does not include. The module cache is left out: re-downloading it
# takes about 20s, compiling is what the build cache saves.
#
#   go-cache-mounts.sh inject  DIR   seed the mount from DIR/go-build.tar
#   go-cache-mounts.sh extract DIR   write the mount out to DIR/go-build.tar
set -euo pipefail

mode=$1
dir=$2
mount='--mount=type=cache,target=/root/.cache/go-build'
work=$(mktemp -d)
# --no-cache would also give the RUN step a fresh, empty cache mount, so a
# timestamp file busts the step cache instead. The cache travels as one tar:
# exporting its many small files to a local directory stalls BuildKit.
date +%s%N > "$work/buildstamp"

case "$mode" in
inject)
  mkdir -p "$dir"
  if [ ! -f "$dir/go-build.tar" ]; then
    echo "no saved Go build cache; starting cold"
    exit 0
  fi
  cp "$work/buildstamp" "$dir/buildstamp"
  cat > "$work/Dockerfile" <<DOCKERFILE
FROM busybox:1
COPY buildstamp /buildstamp
RUN $mount --mount=type=bind,target=/seed tar -xf /seed/go-build.tar -C /root/.cache/go-build
DOCKERFILE
  docker buildx build --progress=plain -f "$work/Dockerfile" "$dir"
  ;;
extract)
  cat > "$work/Dockerfile" <<DOCKERFILE
FROM busybox:1 AS copy
COPY buildstamp /buildstamp
RUN $mount mkdir -p /out && tar -cf /out/go-build.tar -C /root/.cache/go-build .
FROM scratch
COPY --from=copy /out /
DOCKERFILE
  rm -rf "$dir"
  docker buildx build --progress=plain -f "$work/Dockerfile" \
    --output "type=local,dest=$dir" "$work"
  ls -lh "$dir"
  ;;
*)
  echo "usage: $0 inject|extract DIR" >&2
  exit 2
  ;;
esac
