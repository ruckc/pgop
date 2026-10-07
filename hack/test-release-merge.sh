#!/usr/bin/env bash
# Tests for hack/release-merge.sh.
#
# Builds a throwaway git repo that mimics the chart layout, cuts overlapping
# release branches (the scenario from issue #21) and checks that merging them
# back leaves no conflict markers and keeps the higher version.
#
# Usage: hack/test-release-merge.sh
# Runs `helm lint` on the result as well when helm is on PATH.
set -euo pipefail

SCRIPT="${RELEASE_MERGE_SCRIPT:-$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/release-merge.sh}"
WORK="$(mktemp -d)"
trap 'rm -rf "${WORK}"' EXIT

FAILURES=0
fail() {
  echo "FAIL: $*" >&2
  FAILURES=$((FAILURES + 1))
}

git_q() { git "$@" >/dev/null 2>&1; }

# stamp <version>: what prepare-release does on a release branch.
stamp() {
  local version="$1"
  echo "${version}" > VERSION
  sed -i "s/^version:.*/version: ${version#v}/" charts/pgop/Chart.yaml
  sed -i "s/^appVersion:.*/appVersion: \"${version}\"/" charts/pgop/Chart.yaml
  cp config/crd/bases/*.yaml charts/pgop/crds/
  git add VERSION charts/pgop/Chart.yaml charts/pgop/crds/
  git_q commit -m "chore: release ${version}"
}

# new_repo <dir>: main at v0.4.5 with a feature commit on top.
new_repo() {
  local dir="$1"
  mkdir -p "${dir}"
  cd "${dir}"
  git_q init -b main
  git config user.name test
  git config user.email test@example.com
  git config commit.gpgsign false
  git config tag.gpgsign false
  mkdir -p charts/pgop/crds charts/pgop/templates config/crd/bases
  echo "v0.4.5" > VERSION
  cat > charts/pgop/Chart.yaml <<'EOF'
apiVersion: v2
name: pgop
description: test chart
type: application
version: 0.4.5
appVersion: "v0.4.5"
EOF
  cat > config/crd/bases/test.yaml <<'EOF'
apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: tests.example.com
EOF
  cp config/crd/bases/test.yaml charts/pgop/crds/
  echo "# readme" > README.md
  git add -A
  git_q commit -m "initial"
  git_q tag v0.4.5
}

assert_clean_result() {
  local want="$1" label="$2"
  if grep -nE '^(<<<<<<<|=======$|>>>>>>>)' VERSION charts/pgop/Chart.yaml; then
    fail "${label}: conflict markers left in VERSION/Chart.yaml"
  fi
  if [ -n "$(git diff --name-only --diff-filter=U)" ]; then
    fail "${label}: unmerged paths remain"
  fi
  if [ -n "$(git status --porcelain)" ]; then
    fail "${label}: working tree not clean after merge"
  fi
  [ "$(cat VERSION)" = "${want}" ] || fail "${label}: VERSION is '$(cat VERSION)', want '${want}'"
  grep -qx "version: ${want#v}" charts/pgop/Chart.yaml ||
    fail "${label}: Chart.yaml version is not ${want#v}"
  grep -qx "appVersion: \"${want}\"" charts/pgop/Chart.yaml ||
    fail "${label}: Chart.yaml appVersion is not ${want}"
  [ "$(grep -c '^version:' charts/pgop/Chart.yaml)" = 1 ] ||
    fail "${label}: Chart.yaml has more than one version line"
  if command -v helm >/dev/null 2>&1; then
    helm lint charts/pgop --strict >/dev/null || fail "${label}: helm lint failed"
  fi
}

# Overlapping releases: v0.4.6 and v0.4.7 are cut from different main
# commits, then merged back in the given order. The second merge conflicts.
run_overlap() {
  local first="$1" second="$2" label="$3"
  new_repo "${WORK}/${label}"
  git_q checkout -b release/v0.4.6
  stamp v0.4.6
  git_q checkout main
  echo "feature" >> README.md
  git_q commit -am "feat: something"
  git_q checkout -b release/v0.4.7
  stamp v0.4.7
  git_q checkout main

  "${SCRIPT}" "${first}" "release/${first}" >/dev/null 2>&1 ||
    fail "${label}: merging ${first} failed"
  "${SCRIPT}" "${second}" "release/${second}" >/dev/null 2>&1 ||
    fail "${label}: merging ${second} failed"
  assert_clean_result v0.4.7 "${label}"
  grep -qx "feature" README.md || fail "${label}: non-version change from main was lost"
}

# A conflict outside the version stamp must not be auto-resolved.
run_unexpected_conflict() {
  local label="unexpected-conflict"
  new_repo "${WORK}/${label}"
  git_q checkout -b release/v0.4.6
  echo "release side" > README.md
  git_q commit -am "docs: edit readme on release branch"
  stamp v0.4.6
  git_q checkout main
  echo "main side" > README.md
  git_q commit -am "docs: edit readme"
  stamp v0.4.7

  if "${SCRIPT}" v0.4.6 release/v0.4.6 >/dev/null 2>&1; then
    fail "${label}: merge succeeded despite a conflict in README.md"
  fi
  if git rev-parse -q --verify MERGE_HEAD >/dev/null; then
    git_q merge --abort
  fi
}

run_overlap v0.4.6 v0.4.7 "lower-then-higher"
run_overlap v0.4.7 v0.4.6 "higher-then-lower"
run_unexpected_conflict

if [ "${FAILURES}" -ne 0 ]; then
  echo "${FAILURES} check(s) failed" >&2
  exit 1
fi
echo "release-merge tests passed"
