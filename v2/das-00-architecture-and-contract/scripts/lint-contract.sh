#!/bin/sh
# Lint the das-v1 contract (needs Node 18+ and npm registry access; tools are fetched by npx).
#   sh scripts/lint-contract.sh
set -eu
cd "$(dirname "$0")/.."
echo "== OpenAPI";  npx -y @redocly/cli@1 lint contract/openapi.yaml
echo "== AsyncAPI"; npx -y @asyncapi/cli@2 validate contract/asyncapi.yaml
echo "== protobuf"; (cd contract/proto && npx -y @bufbuild/buf@1 lint && npx -y @bufbuild/buf@1 build)
# Before a release, also: (cd contract/proto && npx @bufbuild/buf breaking --against '<previous release>.git#subdir=contract/proto')
