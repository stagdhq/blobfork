#!/bin/sh
# Starts versitygw inside, over the files in /data and checking signatures
# with the app's key, and blobfork in front of it; when either stops, so does
# the container.
set -e

export BLOBFORK_VERSITYGW="${BLOBFORK_VERSITYGW:-http://127.0.0.1:7070}"

# Buckets are directories of /data/buckets; their objects' metadata is kept
# beside them in /data/meta, so the filesystem needn't take extended
# attributes.
mkdir -p /data/buckets /data/meta

ROOT_ACCESS_KEY="${BLOBFORK_ACCESS_KEY_ID:-test}" \
ROOT_SECRET_KEY="${BLOBFORK_SECRET_ACCESS_KEY:-test}" \
VGW_REGION="${BLOBFORK_REGION:-us-east-1}" \
  versitygw --port 127.0.0.1:7070 --quiet \
    posix --sidecar /data/meta /data/buckets &
versitygw=$!

blobfork s3 serve "$@" &
blobfork=$!

trap 'kill $versitygw $blobfork 2>/dev/null' INT TERM

while kill -0 $versitygw 2>/dev/null && kill -0 $blobfork 2>/dev/null; do
  sleep 1
done

kill $versitygw $blobfork 2>/dev/null || true
wait $blobfork || exit $?
