#!/bin/sh
set -e
HASH=$(git rev-parse --short HEAD 2>/dev/null || echo "unknown")
CGO_ENABLED=0 go build -ldflags "-X main.gitHash=$HASH" -o "${1:-mygnoscan}" .
