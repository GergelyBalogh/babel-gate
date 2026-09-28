#!/usr/bin/env sh
# Run Laya natively in a virtualenv (uses Apple MPS or CUDA when available).
#   deploy/laya/run.sh            # english checkpoint on :8000
#   LAYA_MODELS=multilingual deploy/laya/run.sh
set -e
dir=$(cd "$(dirname "$0")" && pwd)
venv="$dir/.venv"
if [ ! -x "$venv/bin/laya-serve" ]; then
  python3 -m venv "$venv"
  "$venv/bin/pip" install --upgrade pip
  "$venv/bin/pip" install "laya[serve]==${LAYA_VERSION:-0.3.21}"
fi
export LAYA_HOST="${LAYA_HOST:-127.0.0.1}"
export LAYA_MODELS="${LAYA_MODELS:-english}"
export LAYA_MAX_LOADED="${LAYA_MAX_LOADED:-1}"
exec "$venv/bin/laya-serve"
