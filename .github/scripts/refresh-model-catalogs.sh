#!/usr/bin/env bash
# Manual catalog refresh: copies models.json and codex_client_models.json from a
# models repository into internal/registry/models for review in a PR. CI builds
# and running proxies only use the catalogs committed to this repo's main.
set -euo pipefail

models_repository="${MODELS_REPOSITORY_URL:-https://github.com/router-for-me/models.git}"
models_ref="${MODELS_REPOSITORY_REF:-main}"
catalog_dir="${MODEL_CATALOG_DIR:-internal/registry/models}"
codex_catalog="$catalog_dir/codex_client_models.json"
codex_candidate="$(mktemp)"
trap 'rm -f "$codex_candidate"' EXIT

git fetch --depth 1 "$models_repository" "$models_ref"
git show FETCH_HEAD:models.json > "$catalog_dir/models.json"

if git show FETCH_HEAD:codex_client_models.json > "$codex_candidate" &&
  go run ./cmd/validate_codex_models --file "$codex_candidate"; then
	mv "$codex_candidate" "$codex_catalog"
	printf 'Refreshed validated Codex client model catalog.\n'
else
	printf '::warning::Remote Codex client model catalog is missing or invalid; using embedded fallback.\n'
fi

go run ./cmd/validate_codex_models --file "$codex_catalog"
