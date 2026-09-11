#!/bin/bash
#
set -xeuo pipefail

SCRIPT_DIR=$(cd $(dirname $0); pwd)
pushd $SCRIPT_DIR/..
node -c internal/app/static/app.js
go test -v ./...
npm run test:e2e
popd
