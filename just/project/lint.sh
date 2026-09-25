#!/usr/bin/env bash
set -euo pipefail

case "${1:-}" in
    check|correct) ;;
    *) echo "Error: lint mode must be check or correct" >&2; exit 1 ;;
esac

goreleaser check
nix flake check --no-write-lock-file
