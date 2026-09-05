#!/bin/bash
#
SCRIPT_DIR=$(cd $(dirname $0); pwd)
pushd $SCRIPT_DIR/..
go test -v ./...
npm run test:e2e
popd
