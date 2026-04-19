#!/usr/bin/env bash
set -e

ROOT="$(dirname "$0")/../.."

go build -o "$ROOT/gateway" "$ROOT/cmd/gateway"

set -a
source "$(dirname "$0")/.env"
set +a

exec "$ROOT/gateway"
