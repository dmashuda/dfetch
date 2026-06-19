#!/usr/bin/env bash
# Generates the ANTLR Go parser for dfetch's SQLite grammar.
#
# Requires Java (to run the ANTLR tool). The generated code is committed, so this
# is only needed when the grammar or ANTLR version changes — normal builds do not
# run it.
#
# The ANTLR tool version MUST match the antlr4-go runtime version in go.mod.
set -euo pipefail

ANTLR_VERSION="4.13.1"
JAR_NAME="antlr-${ANTLR_VERSION}-complete.jar"
CACHE_DIR="${ANTLR_CACHE_DIR:-$HOME/.cache/antlr}"
JAR_PATH="${ANTLR_JAR:-$CACHE_DIR/$JAR_NAME}"

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
grammar_dir="$repo_root/internal/sqlparse/grammar"
out_dir="$repo_root/internal/sqlparse/gen"

if ! command -v java >/dev/null 2>&1; then
  echo "error: java is required to generate the parser (install a JDK)" >&2
  exit 1
fi

if [[ ! -f "$JAR_PATH" ]]; then
  echo "downloading $JAR_NAME -> $JAR_PATH"
  mkdir -p "$(dirname "$JAR_PATH")"
  curl -fsSL "https://www.antlr.org/download/$JAR_NAME" -o "$JAR_PATH"
fi

echo "generating parser into internal/sqlparse/gen (package gen)"
rm -rf "$out_dir"
mkdir -p "$out_dir"

# Compile lexer + parser together so tokenVocab=SQLiteLexer resolves.
( cd "$grammar_dir" && java -jar "$JAR_PATH" \
    -Dlanguage=Go \
    -package gen \
    -listener -no-visitor \
    -o "$out_dir" \
    SQLiteLexer.g4 SQLiteParser.g4 )

# Drop ANTLR's non-Go interpreter artifacts; keep only the .go sources.
find "$out_dir" -type f ! -name '*.go' -delete

echo "done. generated files:"
ls -1 "$out_dir"
