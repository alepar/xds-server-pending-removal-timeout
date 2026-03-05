#!/usr/bin/env bash
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$SCRIPT_DIR"
echo "Building test-runner..."
go build -o test-runner .
echo "Running tests..."
exec ./test-runner "$@"
