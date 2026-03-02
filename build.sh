#!/bin/bash
set -e

APP="justproxy"

case "${1:-prod}" in
  debug)
    echo "Building debug..."
    go build -gcflags="all=-N -l" -o "$APP-debug" .
    echo "Built $APP-debug"
    ;;
  prod|production)
    echo "Building production..."
    CGO_ENABLED=0 go build -ldflags="-s -w" -trimpath -o "$APP" .
    echo "Built $APP"
    ;;
  *)
    echo "Usage: $0 {debug|prod}"
    exit 1
    ;;
esac
