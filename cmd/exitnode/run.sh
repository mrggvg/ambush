#!/usr/bin/env bash
set -e

ROOT="$(dirname "$0")/../.."

go build -o "$ROOT/exitnode" "$ROOT/cmd/exitnode"

exec "$ROOT/exitnode"
