#!/bin/sh

set -eu

project="fast-fotos-playwright"
compose="docker compose -p $project -f compose.yaml -f compose.playwright.yaml"

cleanup() {
  $compose down --volumes
}

trap cleanup EXIT INT TERM
$compose up --build --detach

while :; do
  sleep 1
done
