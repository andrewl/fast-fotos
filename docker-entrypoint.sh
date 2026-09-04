#!/bin/sh

set -eu

geonames_directory="${GEONAMES_DIR:-}"
object_model_directory="${OBJECT_MODEL_DIR:-}"

if [ -n "$geonames_directory" ]; then
  chown appuser:appuser "$geonames_directory"
fi

if [ "${GEONAMES_AUTO_INSTALL:-true}" = "true" ] && [ -n "$geonames_directory" ]; then
  gosu appuser /usr/local/bin/install-geonames.sh "$geonames_directory"
fi

if [ -n "$object_model_directory" ]; then
  mkdir -p "$object_model_directory"
  chown appuser:appuser "$object_model_directory"
fi
if [ "${OBJECT_AUTO_INSTALL:-true}" = "true" ] && [ -n "$object_model_directory" ]; then
  gosu appuser /usr/local/bin/install-onnx.sh "$object_model_directory"
fi

exec gosu appuser /usr/local/bin/fast-fotos
