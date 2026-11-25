#!/bin/bash

check_github_release() {
  local repo=$1
  local current=$2
  local latest=$(curl -s "https://api.github.com/repos/$repo/releases/latest" | grep -i '"tag_name":' | sed 's/.*"\(.*\)",/\1/')
  if [ -n "$latest" ] && [ "$latest" != "$current" ]; then
    echo "$repo: $current -> $latest"
  fi
}

check_github_action() {
  local action=$1
  local current=$2
  local latest=$(curl -s "https://api.github.com/repos/$action/releases/latest" | grep -i '"tag_name":' | sed 's/.*"\(.*\)",/\1/')
  if [ -n "$latest" ] && [ "$latest" != "$current" ]; then
    echo "$action: $current -> $latest"
  fi
}

echo "--- Go Modules ---"
check_github_release "lib/pq" "v1.12.3"
check_github_release "onsi/ginkgo" "v2.29.0"
check_github_release "onsi/gomega" "v1.40.0"
check_github_release "xdg-go/stringprep" "v1.0.4"
check_github_release "kubernetes-sigs/controller-runtime" "v0.24.1"
check_github_release "kubernetes-sigs/yaml" "v1.6.0"
# k8s.io/api, etc. are in kubernetes/kubernetes
check_github_release "kubernetes/kubernetes" "v0.36.1"

echo "--- GitHub Actions ---"
check_github_action "actions/checkout" "v6.0.3"
check_github_action "actions/setup-go" "v6.4.0"
check_github_action "golangci/golangci-lint-action" "v9.2.1"
check_github_action "actions/cache" "v5.0.5"
check_github_action "actions/setup-python" "v6.2.0"
check_github_action "actions/configure-pages" "v6.0.0"
check_github_action "actions/upload-pages-artifact" "v5.0.0"
check_github_action "actions/deploy-pages" "v5.0.0"
check_github_action "docker/setup-buildx-action" "v4.1.0"
check_github_action "docker/build-push-action" "v7.2.0"
check_github_action "docker/login-action" "v4.2.0"

echo "--- Other ---"
check_github_release "pgbackrest/pgbackrest" "2.59.3"
