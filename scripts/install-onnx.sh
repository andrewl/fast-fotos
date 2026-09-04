#!/bin/sh
set -eu
destination="${1:-${OBJECT_MODEL_DIR:-/models}}"
runtime_version="${ONNX_RUNTIME_VERSION:-1.29.0}"
case "$(uname -m)" in
  x86_64) runtime_arch="x64" ;;
  aarch64) runtime_arch="aarch64" ;;
  *) printf 'Unsupported ONNX Runtime architecture: %s\n' "$(uname -m)" >&2; exit 1 ;;
esac
model_name="yolo11s.onnx"
model_url="https://github.com/ultralytics/assets/releases/download/v8.4.0/${model_name}"
runtime_url="https://github.com/microsoft/onnxruntime/releases/download/v${runtime_version}/onnxruntime-linux-${runtime_arch}-${runtime_version}.tgz"
temporary_directory="$destination/.tmp.$$"
cleanup() { rm -rf "$temporary_directory"; }
trap cleanup EXIT INT TERM
mkdir -p "$destination" "$temporary_directory"
rm -f "$destination/ssd-12.onnx" "$destination/yolov8n.onnx" "$destination/yolo11m.onnx"
if [ ! -f "$destination/$model_name" ]; then
  curl --fail --location --show-error --silent --output "$temporary_directory/$model_name" "$model_url"
  mv "$temporary_directory/$model_name" "$destination/$model_name"
fi
if [ ! -f "$destination/libonnxruntime.so.${runtime_version}" ]; then
  curl --fail --location --show-error --silent --output "$temporary_directory/onnxruntime.tgz" "$runtime_url"
  tar -xzf "$temporary_directory/onnxruntime.tgz" -C "$temporary_directory"
  rm -f "$destination"/libonnxruntime.so.*
  mv "$temporary_directory/onnxruntime-linux-${runtime_arch}-${runtime_version}/lib/libonnxruntime.so.${runtime_version}" "$destination/libonnxruntime.so.${runtime_version}"
fi
printf 'ONNX object-detection assets are installed in %s\n' "$destination"
