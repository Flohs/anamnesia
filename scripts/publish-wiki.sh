#!/usr/bin/env bash
#
# Publish docs/wiki/ to the GitHub wiki.
#
# A GitHub wiki is a separate git repository (anamnesia.wiki.git) from the
# code. Nothing committed to main ever reaches it, which is why the wiki
# stays empty however many times docs/wiki/ changes. This script is the
# bridge: docs/wiki/ stays the source of truth, reviewed in the same diff as
# the code it describes, and the wiki is a published mirror of it.
#
# BOOTSTRAP, ONCE: GitHub creates anamnesia.wiki.git lazily, the first time a
# page is saved through the web UI. There is no API for it. Until someone
# does that, this script cannot clone anything and says so. Go to
# https://github.com/Flohs/anamnesia/wiki, click "Create the first page",
# save anything at all, then run this. The placeholder gets overwritten.
#
# Usage:
#   scripts/publish-wiki.sh              # transform, diff, push
#   scripts/publish-wiki.sh --dry-run    # transform and print, push nothing
set -euo pipefail

REPO_SLUG="Flohs/anamnesia"
BLOB="https://github.com/${REPO_SLUG}/blob/main"
WIKI_REMOTE="git@github.com:${REPO_SLUG}.wiki.git"

DRY_RUN=0
[[ "${1:-}" == "--dry-run" ]] && DRY_RUN=1

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
src="$root/docs/wiki"
[[ -d "$src" ]] || { echo "no docs/wiki at $src" >&2; exit 1; }

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
staged="$work/pages"
mkdir -p "$staged"

# rewrite_root FILE — links as they appear in docs/wiki/*.md
rewrite_root() {
  perl -0777 -pe '
    s{\]\(reference/([a-z0-9-]+)\.md}{](reference-$1}g;
    s{\]\(\.\./\.\./([A-Za-z0-9._-]+\.md)\)}{](BLOB_URL/$1)}g;
    s{\]\(\.\./([a-z0-9-]+\.md)\)}{](BLOB_URL/docs/$1)}g;
    s{\]\(([a-z0-9-]+)\.md}{]($1}g;
  ' "$1" | sed "s|BLOB_URL|$BLOB|g"
}

# rewrite_reference FILE — links as they appear in docs/wiki/reference/*.md
rewrite_reference() {
  perl -0777 -pe '
    s{\]\((?:\.\./){3}([A-Za-z0-9._-]+\.md)\)}{](BLOB_URL/$1)}g;
    s{\]\(\.\./\.\./([A-Za-z0-9._-]+\.md)\)}{](BLOB_URL/$1)}g;
    s{\]\(\.\./([a-z0-9-]+)\.md}{]($1}g;
    s{\]\(([a-z0-9-]+)\.md}{](reference-$1}g;
  ' "$1" | sed "s|BLOB_URL|$BLOB|g"
}

echo "Transforming docs/wiki -> wiki pages"

for f in "$src"/*.md; do
  base="$(basename "$f" .md)"
  # README is the wiki's landing page, which GitHub requires be named Home.
  [[ "$base" == "README" ]] && out="Home" || out="$base"
  rewrite_root "$f" > "$staged/$out.md"
  echo "  $(basename "$f")  ->  $out.md"
done

for f in "$src"/reference/*.md; do
  out="reference-$(basename "$f" .md)"
  rewrite_reference "$f" > "$staged/$out.md"
  echo "  reference/$(basename "$f")  ->  $out.md"
done

# The sidebar is wiki-only navigation: docs/wiki/README.md serves that role
# in the repository, where a directory listing is right there.
cat > "$staged/_Sidebar.md" <<EOF
### [Anamnesia](Home)

**Guides**
- [Getting started](getting-started)
- [Configuring](configuration)
- [Memory model](memory-model)
- [Hooks](hooks)
- [Extraction](extraction)
- [Retrieval](retrieval)
- [Troubleshooting](troubleshooting)
- [Architecture](architecture)

**Reference**
- [Configuration](reference-config)
- [CLI](reference-cli)
- [MCP tools](reference-mcp-tools)
- [HTTP API](reference-http-api)

---
[Repository](https://github.com/${REPO_SLUG})
EOF

cat > "$staged/_Footer.md" <<EOF
Generated from [\`docs/wiki/\`](${BLOB}/docs/wiki) by \`scripts/publish-wiki.sh\`. Edit there, not here: edits made in this wiki are overwritten on the next publish.
EOF

echo "  (generated _Sidebar.md, _Footer.md)"

# Any relative .md link left over is one the rewriting missed, and would be a
# dead link on the wiki. Fail rather than publish it.
if leftover="$(grep -rnoE '\]\([^)]*\.md[^)]*\)' "$staged" | grep -v 'https://' || true)"; then
  if [[ -n "$leftover" ]]; then
    echo >&2
    echo "Unrewritten .md links would be dead on the wiki:" >&2
    echo "$leftover" >&2
    exit 1
  fi
fi
echo "  no unrewritten .md links"

if [[ $DRY_RUN -eq 1 ]]; then
  echo
  echo "--dry-run: pages are in $staged (kept below), nothing pushed"
  keep="$root/.wiki-preview"
  rm -rf "$keep" && cp -R "$staged" "$keep"
  echo "  copied to $keep"
  trap - EXIT; rm -rf "$work"
  exit 0
fi

echo
echo "Cloning $WIKI_REMOTE"
if ! git clone -q "$WIKI_REMOTE" "$work/wiki" 2>"$work/err"; then
  echo >&2
  echo "Could not clone the wiki repository." >&2
  sed 's/^/  /' "$work/err" >&2
  echo >&2
  echo "If this says 'Repository not found', the wiki has never been" >&2
  echo "initialised. GitHub creates it the first time a page is saved in" >&2
  echo "the web UI, and offers no API for it. Do this once:" >&2
  echo >&2
  echo "  1. open https://github.com/${REPO_SLUG}/wiki" >&2
  echo "  2. click 'Create the first page' and save it (any content)" >&2
  echo "  3. run this script again; the placeholder is overwritten" >&2
  exit 1
fi

# Replace the page set wholesale so a renamed or deleted page does not
# linger. Keeps .git, drops everything else.
find "$work/wiki" -mindepth 1 -maxdepth 1 ! -name .git -exec rm -rf {} +
cp "$staged"/*.md "$work/wiki/"

cd "$work/wiki"
if git diff --quiet && git diff --cached --quiet && [[ -z "$(git status --porcelain)" ]]; then
  echo "Wiki already matches docs/wiki. Nothing to push."
  exit 0
fi

git add -A
git commit -q -m "Publish docs/wiki at $(git -C "$root" rev-parse --short HEAD)"
git push -q origin HEAD
echo "Pushed. https://github.com/${REPO_SLUG}/wiki"
