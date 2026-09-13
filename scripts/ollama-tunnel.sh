#!/usr/bin/env bash
# SSH port-forward to the remote Ollama on 192.168.85.248 (bound to its
# localhost). Exposes it locally as http://127.0.0.1:11435/v1.
#
# A plain TCP relay was tried and abandoned: it mishandled HTTP keep-alive
# (stale upstream connections caused intermittent EOFs). ssh -L is
# connection-oriented and handles this correctly.
#
# Requires: sshpass (or configure an SSH key for lz@192.168.85.248).
set -euo pipefail

REMOTE_USER_HOST="${OLLAMA_SSH:-lz@192.168.85.248}"
LOCAL_PORT="${OLLAMA_LOCAL_PORT:-11435}"

if ss -tln | grep -q "127.0.0.1:${LOCAL_PORT} "; then
    echo "port ${LOCAL_PORT} already forwarded"
    exit 0
fi

exec ssh -N \
    -L "${LOCAL_PORT}:127.0.0.1:11434" \
    -o StrictHostKeyChecking=no \
    -o ServerAliveInterval=30 \
    -o ServerAliveCountMax=3 \
    -o ExitOnForwardFailure=yes \
    "$REMOTE_USER_HOST"
