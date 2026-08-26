#!/usr/bin/env bash
# Rebuild the EPUB from these source files. Requires pandoc 3.x.
#   ./build.sh                 # -> Microservices-in-Go.epub
set -euo pipefail

pandoc \
  metadata.yaml \
  00-about.md \
  01-why-microservices.md \
  02-first-service.md \
  03-docker.md \
  04-compose.md \
  05-data.md \
  06-messaging.md \
  07-testing.md \
  08-cicd.md \
  09-kubernetes.md \
  10-helm.md \
  11-terraform.md \
  12-gitops.md \
  13-observability.md \
  14-production.md \
  15-appendix.md \
  --toc --toc-depth=2 --split-level=1 \
  --css epub.css \
  --epub-cover-image images/cover.png \
  --resource-path=.:images \
  --highlight-style=tango \
  -o "Microservices-in-Go.epub"

echo "Wrote Microservices-in-Go.epub"
