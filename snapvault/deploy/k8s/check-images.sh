#!/usr/bin/env bash
# Render the prod overlay and fail if any Deployment still uses a base
# placeholder image (snapvault/<svc>). The overlay's `images:` list, and CI's
# `kustomize edit set image snapvault/<svc>=...`, only rewrite images whose
# name matches the base exactly; a mismatch is silent and ships the
# placeholder. Run from snapvault/:  deploy/k8s/check-images.sh
set -euo pipefail
cd "$(dirname "$0")/../.."

KUSTOMIZE=sigs.k8s.io/kustomize/kustomize/v5@v5.4.3
rendered=$(GOFLAGS= go run "$KUSTOMIZE" build deploy/k8s/overlays/prod)

if grep -nE '^[[:space:]]*image: snapvault/' <<<"$rendered"; then
  echo "error: the placeholder image(s) above have no mapping in deploy/k8s/overlays/prod" >&2
  exit 1
fi
echo "ok: every image in the prod overlay is mapped"
