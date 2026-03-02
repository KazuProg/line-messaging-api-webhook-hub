#!/bin/sh
set -e
# ボリュームマウント先を appuser が書き込めるようにする
if [ -d /data ]; then
  chown -R appuser:appuser /data
fi
exec su appuser -c 'exec /app/webhook-hub'
