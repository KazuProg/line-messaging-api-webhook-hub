#!/bin/sh
set -e

if [ -d /data ]; then
  chown -R appuser:appuser /data
fi

exec su appuser -c 'exec /app/webhook-hub'
