#!/usr/bin/env bash
set -euo pipefail

exact_tag="$(git describe --tags --match 'v[0-9]*' --exact-match 2>/dev/null || true)"
if [[ -n "${exact_tag}" ]]; then
  printf '%s\n' "${exact_tag#v}"
  exit 0
fi

latest_tag="$(git describe --tags --match 'v[0-9]*' --abbrev=0 2>/dev/null || true)"
if [[ -z "${latest_tag}" ]]; then
  count="$(git rev-list --count HEAD 2>/dev/null || printf '0')"
  printf '0.1.0-dev.%s\n' "${count}"
  exit 0
fi

base="${latest_tag#v}"
distance="$(git rev-list --count "${latest_tag}..HEAD" 2>/dev/null || printf '0')"
IFS='.' read -r major minor patch <<EOF
${base}
EOF
if [[ -z "${major:-}" || -z "${minor:-}" || -z "${patch:-}" ]]; then
  printf '%s-dev.%s\n' "${base}" "${distance}"
  exit 0
fi

next_patch=$((patch + 1))
printf '%s.%s.%s-dev.%s\n' "${major}" "${minor}" "${next_patch}" "${distance}"
