#!/usr/bin/env bash
set -e
ROOT="$(dirname "$0")/../.."
go build -o "$ROOT/gencerts" "$ROOT/cmd/gencerts"
exec "$ROOT/gencerts" "$@"
