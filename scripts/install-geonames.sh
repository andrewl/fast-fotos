#!/bin/sh

set -eu

destination="${1:-${GEONAMES_DIR:-geonames}}"
base_url="https://download.geonames.org/export/dump"
temporary_directory="$destination/.tmp.$$"

cleanup() {
  rm -rf "$temporary_directory"
}

trap cleanup EXIT INT TERM

mkdir -p "$destination" "$temporary_directory"

download() {
  name="$1"
  url="$2"
  target="$temporary_directory/$name"
  if [ -f "$destination/$name" ]; then
    printf 'Keeping existing %s/%s\n' "$destination" "$name"
    return
  fi
  printf 'Downloading %s\n' "$name"
  curl --fail --location --show-error --silent --output "$target" "$url"
}

download "cities500.zip" "$base_url/cities500.zip"
download "admin1CodesASCII.txt" "$base_url/admin1CodesASCII.txt"
download "admin2Codes.txt" "$base_url/admin2Codes.txt"
download "countryInfo.txt" "$base_url/countryInfo.txt"

for name in cities500.zip admin1CodesASCII.txt admin2Codes.txt countryInfo.txt; do
  if [ -f "$temporary_directory/$name" ]; then
    mv "$temporary_directory/$name" "$destination/$name"
  fi
done

printf 'GeoNames data is installed in %s\n' "$destination"
