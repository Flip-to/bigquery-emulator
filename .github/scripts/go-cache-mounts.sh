#!/usr/bin/env bash
# Moves the Dockerfile's Go cache mounts (/go/pkg/mod, /root/.cache/go-build)
# in and out of BuildKit, so actions/cache can persist them between runs.
# The type=gha layer cache does not export RUN --mount=type=cache contents.
# Same technique as reproducible-containers/buildkit-cache-dance, which the
# org's action allowlist does not include.
#
#   go-cache-mounts.sh inject  DIR   seed the mounts from DIR/{mod,build}
#   go-cache-mounts.sh extract DIR   copy the mounts back out to DIR
set -euo pipefail

mode=$1
dir=$2
mounts='--mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build'
work=$(mktemp -d)

case "$mode" in
inject)
  mkdir -p "$dir/mod" "$dir/build"
  cat > "$work/Dockerfile" <<DOCKERFILE
FROM busybox:1
COPY . /seed
RUN $mounts cp -a /seed/mod/. /go/pkg/mod/ && cp -a /seed/build/. /root/.cache/go-build/ && du -sh /go/pkg/mod /root/.cache/go-build
DOCKERFILE
  docker buildx build --no-cache --progress=plain -f "$work/Dockerfile" "$dir"
  ;;
extract)
  docker buildx du --verbose --filter type=exec.cachemount || true
  cat > "$work/Dockerfile" <<DOCKERFILE
FROM busybox:1 AS copy
RUN $mounts du -sh /go/pkg/mod /root/.cache/go-build && mkdir -p /out && cp -a /go/pkg/mod /out/mod && cp -a /root/.cache/go-build /out/build
FROM scratch
COPY --from=copy /out /
DOCKERFILE
  # Module cache files are read-only; make them removable first.
  if [ -d "$dir" ]; then chmod -R u+w "$dir"; rm -rf "$dir"; fi
  docker buildx build --no-cache --progress=plain -f "$work/Dockerfile" \
    --output "type=local,dest=$dir" "$work"
  du -sh "$dir"/*
  ;;
*)
  echo "usage: $0 inject|extract DIR" >&2
  exit 2
  ;;
esac
