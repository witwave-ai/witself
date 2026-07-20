#!/usr/bin/env bash
set -euo pipefail

state_dir=${WITSELF_TEST_DOCKER_STATE_DIR:?}
log=${WITSELF_TEST_DOCKER_LOG:?}
version=${WITSELF_TEST_DOCKER_VERSION:?}
cli_digest=${WITSELF_TEST_WITSELF_DIGEST:?}
server_digest=${WITSELF_TEST_SERVER_DIGEST:?}
cli_image=ghcr.io/witwave-ai/images/witself
server_image=ghcr.io/witwave-ai/images/witself-server

printf '%s\n' "$*" >> "$log"

image_details() {
  case $1 in
    "$cli_image":*|"$cli_image"@*)
      image=$cli_image
      key=witself
      expected_digest=$cli_digest
      ;;
    "$server_image":*|"$server_image"@*)
      image=$server_image
      key=witself-server
      expected_digest=$server_digest
      ;;
    *)
      echo "mock docker: unexpected image reference: $1" >&2
      exit 1
      ;;
  esac
}

if (( $# == 6 )) && [[ $1 == buildx && $2 == imagetools && $3 == inspect && $5 == --format && $6 == '{{.Manifest.Digest}}' ]]; then
  reference=$4
  image_details "$reference"
  tag=${reference#"$image:"}

  if [[ $tag == latest ]]; then
    state_file="$state_dir/$key.latest"
    [[ -f $state_file ]] || { echo "mock docker: latest is missing for $image" >&2; exit 1; }
    digest=$(<"$state_file")
    if [[ ${WITSELF_TEST_MISMATCH_IMAGE:-} == "$key" ]]; then
      digest=sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc
    fi
    printf '%s\n' "$digest"
    exit 0
  fi

  [[ $tag == "$version" ]] \
    || { echo "mock docker: unexpected immutable tag: $reference" >&2; exit 1; }
  [[ ${WITSELF_TEST_MISSING_IMAGE:-} != "$key" ]] \
    || { echo "mock docker: immutable image is missing: $reference" >&2; exit 1; }
  printf '%s\n' "$expected_digest"
  exit 0
fi

if (( $# == 6 )) && [[ $1 == buildx && $2 == imagetools && $3 == create && $4 == --tag ]]; then
  target=$5
  source=$6
  image_details "$target"
  [[ $target == "$image:latest" ]] \
    || { echo "mock docker: unexpected promotion target: $target" >&2; exit 1; }
  [[ $source == "$image@$expected_digest" ]] \
    || { echo "mock docker: promotion did not use the immutable digest: $source" >&2; exit 1; }
  printf '%s\n' "$expected_digest" > "$state_dir/$key.latest"
  exit 0
fi

echo "mock docker: unexpected arguments: $*" >&2
exit 1
