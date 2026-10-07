#!/usr/bin/env bash
# Merge a release branch into the currently checked-out branch (main).
#
# Usage: hack/release-merge.sh <version> <release-ref>
#   version      release version, e.g. v1.2.3
#   release-ref  ref of the release branch to merge, e.g. origin/release/v1.2.3
#
# Must be run from the repository root with the target branch checked out and
# git user.name / user.email configured. Does not push.
#
# The release branch only ever bumps the version stamp on top of the main it
# was cut from. When several releases are in flight, main can advance to a
# newer version before this merge runs, so VERSION / Chart.yaml / the vendored
# CRDs conflict. Auto-resolve those files by taking the HIGHER of the two
# versions so main never regresses, and fail loudly on any other (unexpected)
# conflict.
#
# The body is wrapped in main() so bash parses the whole script before running
# it: the merge may rewrite this very file in the working tree.
set -euo pipefail

main() {
  if [ "$#" -ne 2 ]; then
    echo "usage: $0 <version> <release-ref>" >&2
    return 2
  fi

  local VERSION="$1"
  local RELEASE_REF="$2"

  if git merge --no-ff "${RELEASE_REF}" -m "chore: release ${VERSION}"; then
    return 0
  fi

  local MAIN_VERSION HIGHER CHART_VERSION UNMERGED
  MAIN_VERSION=$(git show HEAD:VERSION 2>/dev/null | tr -d '[:space:]' || true)
  MAIN_VERSION="${MAIN_VERSION:-v0.0.0}"
  HIGHER=$(printf '%s\n%s\n' "${MAIN_VERSION#v}" "${VERSION#v}" | sort -V | tail -1)
  HIGHER="v${HIGHER}"
  CHART_VERSION="${HIGHER#v}"

  # Start from main's copy so no conflict markers survive the stamp.
  git checkout --ours -- charts/pgop/Chart.yaml
  echo "${HIGHER}" > VERSION
  sed -i "s/^version:.*/version: ${CHART_VERSION}/" charts/pgop/Chart.yaml
  sed -i "s/^appVersion:.*/appVersion: \"${HIGHER}\"/" charts/pgop/Chart.yaml
  cp config/crd/bases/*.yaml charts/pgop/crds/
  git add VERSION charts/pgop/Chart.yaml charts/pgop/crds/

  if git grep -nE '^(<<<<<<<|=======$|>>>>>>>)' -- VERSION charts/ ':!charts/pgop/templates'; then
    echo "Conflict markers remain after auto-resolution" >&2
    return 1
  fi
  UNMERGED=$(git diff --name-only --diff-filter=U)
  if [ -n "${UNMERGED}" ]; then
    echo "Unresolved merge conflicts outside the version stamp:" >&2
    echo "${UNMERGED}" >&2
    return 1
  fi
  git commit --no-edit
}

main "$@"; exit $?
