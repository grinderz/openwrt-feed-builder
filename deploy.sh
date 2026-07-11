#!/usr/bin/env bash
# Deploy the generated feed tree to the web server.
#
# Usage:
#   ./deploy.sh                 # deploy per deploy.conf
#   ./deploy.sh -n              # dry run
#
# Source and target are read from deploy.conf next to this script.
set -euo pipefail

cd "$(dirname "$0")"

if [[ ! -f deploy.conf ]]; then
    echo "error: deploy.conf not found — copy deploy.conf.example and adjust" >&2
    exit 1
fi
source deploy.conf

SRC="${DEPLOY_SRC:?DEPLOY_SRC not set in deploy.conf}"
TARGET="${DEPLOY_TARGET:?DEPLOY_TARGET not set in deploy.conf}"

if [[ ! -d "$SRC" ]]; then
    echo "error: source directory '$SRC' not found — run the build first" >&2
    exit 1
fi

rsync -aP --delete "$@" "$SRC" "$TARGET"
