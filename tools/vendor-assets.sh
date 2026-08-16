#!/usr/bin/env bash
# Vendors the UI's frontend dependencies into ui/static/vendor/ from npm
# tarballs (npm pack - no CDN involved). Run after bumping a version below,
# then commit the result. The import map in ui/templates/layout.templ and
# this list must stay in sync.
set -euo pipefail

cd "$(dirname "$0")/.."
VENDOR=ui/static/vendor
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

rm -rf "$VENDOR"
mkdir -p "$VENDOR"

# package@version -> flat vendored file (single-file ESM dists)
declare -a SINGLE=(
  "codemirror@6.0.2|codemirror.js"
  "@codemirror/lang-sql@6.10.0|codemirror-lang-sql.js"
  "@codemirror/theme-one-dark@6.1.3|codemirror-theme-one-dark.js"
  "@codemirror/state@6.7.1|codemirror-state.js"
  "@codemirror/view@6.43.8|codemirror-view.js"
  "@codemirror/language@6.12.4|codemirror-language.js"
  "@codemirror/autocomplete@6.20.3|codemirror-autocomplete.js"
  "@codemirror/commands@6.10.4|codemirror-commands.js"
  "@codemirror/search@6.7.1|codemirror-search.js"
  "@codemirror/lint@6.9.7|codemirror-lint.js"
  "@lezer/common@1.5.2|lezer-common.js"
  "@lezer/highlight@1.2.3|lezer-highlight.js"
  "@lezer/lr@1.4.10|lezer-lr.js"
  "@marijn/find-cluster-break@1.0.2|find-cluster-break.js"
  "crelt@1.0.7|crelt.js"
  "style-mod@4.1.3|style-mod.js"
  "w3c-keyname@2.2.8|w3c-keyname.js"
)

fetch() { # $1 pkg@version -> extracted package dir
  local out
  out=$(cd "$WORK" && npm pack "$1" --silent 2>/dev/null | tail -1)
  rm -rf "$WORK/package"
  tar -xzf "$WORK/$out" -C "$WORK"
  echo "$WORK/package"
}

# esm_entry <pkgdir>: resolve the package's ESM entry point
esm_entry() {
  python3 - "$1" <<'EOF'
import json, sys, os
d = sys.argv[1]
p = json.load(open(os.path.join(d, 'package.json')))
entry = None
exp = p.get('exports')
if isinstance(exp, dict):
    dot = exp.get('.', exp)
    if isinstance(dot, dict):
        entry = dot.get('import')
        if isinstance(entry, dict):
            entry = entry.get('default')
entry = entry or p.get('module') or p.get('main')
print(os.path.normpath(os.path.join(d, entry)))
EOF
}

for spec in "${SINGLE[@]}"; do
  pkg="${spec%%|*}"; dest="${spec##*|}"
  dir=$(fetch "$pkg")
  entry=$(esm_entry "$dir")
  # single-file dists must not import relative files - fail loudly if they start to
  if grep -qE "from ['\"]\./" "$entry"; then
    echo "ERROR: $pkg entry has relative imports; vendor the directory instead" >&2
    exit 1
  fi
  cp "$entry" "$VENDOR/$dest"
  echo "vendored $pkg -> $dest"
done

# sql-formatter's ESM tree bare-imports its own deps (nearley), which an
# import map can't resolve without dragging that whole graph in - bundle it
# into one self-contained ESM file instead
mkdir -p "$WORK/sf" && (cd "$WORK/sf" && npm init -y >/dev/null 2>&1 && npm install sql-formatter@15.8.2 --ignore-scripts --silent >/dev/null)
npx -y esbuild "$WORK/sf/node_modules/sql-formatter/dist/esm/index.js" --bundle --format=esm --minify --outfile="$VENDOR/sql-formatter.js" --log-level=error
echo "vendored sql-formatter -> sql-formatter.js (bundled)"

# non-module scripts
dir=$(fetch "htmx.org@2.0.10")
cp "$dir/dist/htmx.min.js" "$VENDOR/htmx.min.js"
echo "vendored htmx.org -> htmx.min.js"

dir=$(fetch "chart.js@4.5.1")
cp "$dir/dist/chart.umd.js" "$VENDOR/chart.umd.js"
echo "vendored chart.js -> chart.umd.js"

echo
du -sh "$VENDOR"
