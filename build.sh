#!/bin/sh
set -e
CGO_ENABLED=0 go build -o "${1:-mygnoscan}" .
