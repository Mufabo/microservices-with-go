#!/usr/bin/env bash
# Rebuild the EPUB and the source bundle from these source files.
#   ./build.sh                 # -> Microservices-in-Go.epub and book-src.zip
#
# Both outputs are reproducible: built with the pinned Pandoc below, the same
# sources produce byte-identical files locally and in CI, so CI only commits
# when the book actually changed.
set -euo pipefail
cd "$(dirname "$0")"

# The Pandoc release this book is built with. CI installs exactly this version
# (.forgejo/workflows/epub.yaml reads it from this line).
PANDOC_VERSION=3.12

# A fixed timestamp for the EPUB's modification date and for every file inside
# both archives. Without it each build embeds the current time and differs.
export SOURCE_DATE_EPOCH="${SOURCE_DATE_EPOCH:-1767225600}" # 2026-01-01T00:00:00Z

have=$(pandoc --version | head -n1 | awk '{print $2}')
if [ "$have" != "$PANDOC_VERSION" ]; then
  echo "warning: pandoc $have found, but the book is pinned to $PANDOC_VERSION;" \
       "the EPUB will not be byte-identical to CI's." >&2
fi

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
  --syntax-highlighting=code-dark.theme \
  -o "Microservices-in-Go.epub"

echo "Wrote Microservices-in-Go.epub"

# book-src.zip: every source file under book-src/, in sorted order, with fixed
# timestamps and permissions. Generated files are left out.
python3 - <<'PY'
import os, time, zipfile

stamp = time.gmtime(int(os.environ["SOURCE_DATE_EPOCH"]))[:6]
skip = {"book-src.zip", "Microservices-in-Go.epub"}

def entry(name, mode):
    info = zipfile.ZipInfo(name, date_time=stamp)
    info.external_attr = mode << 16
    info.compress_type = zipfile.ZIP_DEFLATED
    return info

with zipfile.ZipFile("book-src.zip.tmp", "w") as z:
    for root, dirs, files in os.walk("."):
        dirs.sort()
        rel = os.path.normpath(root)
        prefix = "book-src" if rel == "." else "book-src/" + rel
        z.writestr(entry(prefix + "/", 0o40755), b"")
        for name in sorted(files):
            if rel == "." and (name in skip or name.endswith(".tmp")):
                continue
            path = os.path.join(root, name)
            mode = 0o100755 if name.endswith(".sh") else 0o100644
            with open(path, "rb") as f:
                z.writestr(entry(prefix + "/" + name, mode), f.read())
os.replace("book-src.zip.tmp", "book-src.zip")
PY

echo "Wrote book-src.zip"
