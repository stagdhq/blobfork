#!/bin/sh
# Starts Azurite inside, with the app's account, and blobfork in front of it;
# when either stops, so does the container.
set -e

account="${BLOBFORK_ACCOUNT:-blobfork}"
key="${BLOBFORK_KEY:-Eby8vdM02xNOcqFlqUwJPLlmEtlCDXJ1OUzFT50uSRZ6IFsuFq2UVErCz4I6tq/K1SZFPTOtr/KBHBeksoGMGw==}"

export AZURITE_ACCOUNTS="$account:$key"
export BLOBFORK_AZURITE="${BLOBFORK_AZURITE:-http://127.0.0.1:10010}"

azurite-blob \
  --blobHost 127.0.0.1 \
  --blobPort 10010 \
  --location /data \
  --skipApiVersionCheck \
  --disableProductStyleUrl \
  --silent &
azurite=$!

blobfork serve "$@" &
blobfork=$!

trap 'kill $azurite $blobfork 2>/dev/null' INT TERM

while kill -0 $azurite 2>/dev/null && kill -0 $blobfork 2>/dev/null; do
  sleep 1
done

kill $azurite $blobfork 2>/dev/null || true
wait $blobfork || exit $?
