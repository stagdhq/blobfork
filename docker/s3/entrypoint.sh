#!/bin/sh
# Starts Floci inside, with only S3 in use and checking signatures, and
# blobfork in front of it; when either stops, so does the container.
set -e

export FLOCI_AUTH_VALIDATE_SIGNATURES=true
export BLOBFORK_FLOCI="${BLOBFORK_FLOCI:-http://127.0.0.1:4567}"

# Floci's own entrypoint, which runs it as its own user.
/usr/local/bin/docker-entrypoint.sh /app/application \
  -Dquarkus.http.host=127.0.0.1 \
  -Dquarkus.http.port=4567 &
floci=$!

blobfork s3 serve "$@" &
blobfork=$!

trap 'kill $floci $blobfork 2>/dev/null' INT TERM

while kill -0 $floci 2>/dev/null && kill -0 $blobfork 2>/dev/null; do
  sleep 1
done

kill $floci $blobfork 2>/dev/null || true
wait $blobfork || exit $?
