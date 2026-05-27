#!/usr/bin/env bash
# Cut a release tag. Bumps the latest v* tag and pushes the new one,
# which fires .github/workflows/release.yml on the remote.
#
# Usage: scripts/release.sh patch|minor|major [--yes]
#
#   patch   v0.1.2 -> v0.1.3
#   minor   v0.1.2 -> v0.2.0
#   major   v0.1.2 -> v1.0.0
#
# --yes skips the confirmation prompt.

set -euo pipefail

die() { printf 'error: %s\n' "$*" >&2; exit 1; }

PART="${1:-}"
YES=0
[[ "${2:-}" == "--yes" ]] && YES=1

case "$PART" in
    patch|minor|major) ;;
    *) die "usage: $(basename "$0") patch|minor|major [--yes]" ;;
esac

# Run from repo root.
cd "$(git rev-parse --show-toplevel)"

BRANCH="$(git rev-parse --abbrev-ref HEAD)"
[[ "$BRANCH" == "main" ]] || die "must release from main (on $BRANCH)"

[[ -z "$(git status --porcelain)" ]] || die "working tree not clean — commit or stash first"

git fetch --tags --quiet

# Need remote so the push step works.
git remote get-url origin >/dev/null 2>&1 || die "no 'origin' remote configured"

# Refuse to release a commit the remote doesn't have yet.
LOCAL="$(git rev-parse HEAD)"
REMOTE="$(git rev-parse origin/main 2>/dev/null || echo '')"
[[ -n "$REMOTE" ]] || die "origin/main not found — run 'git fetch origin' first"
if [[ "$LOCAL" != "$REMOTE" ]]; then
    AHEAD="$(git rev-list --count origin/main..HEAD)"
    BEHIND="$(git rev-list --count HEAD..origin/main)"
    die "local main is $AHEAD ahead / $BEHIND behind origin/main — sync first"
fi

LATEST="$(git tag --list 'v*' --sort=-v:refname | head -n1)"
if [[ -z "$LATEST" ]]; then
    MAJ=0; MIN=0; PAT=0
    echo "no existing v* tag — starting from v0.0.0"
else
    [[ "$LATEST" =~ ^v([0-9]+)\.([0-9]+)\.([0-9]+)$ ]] || die "latest tag $LATEST is not vMAJOR.MINOR.PATCH"
    MAJ="${BASH_REMATCH[1]}"
    MIN="${BASH_REMATCH[2]}"
    PAT="${BASH_REMATCH[3]}"
fi

case "$PART" in
    major) MAJ=$((MAJ + 1)); MIN=0; PAT=0 ;;
    minor) MIN=$((MIN + 1)); PAT=0 ;;
    patch) PAT=$((PAT + 1)) ;;
esac

NEW="v${MAJ}.${MIN}.${PAT}"

git rev-parse "$NEW" >/dev/null 2>&1 && die "$NEW already exists"

printf '\nCurrent: %s\nNew:     %s\nCommit:  %s\n\n' \
    "${LATEST:-<none>}" "$NEW" "$(git log -1 --pretty='%h %s')"

if [[ $YES -eq 0 ]]; then
    read -rp "Tag and push $NEW? [y/N] " ans
    [[ "$ans" =~ ^[Yy]$ ]] || die "aborted"
fi

git tag -a "$NEW" -m "$NEW"
git push origin "$NEW"

printf '\npushed %s — CI will build and publish the release.\n' "$NEW"
