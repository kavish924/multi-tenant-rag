#!/usr/bin/env bash
# Fallback dev machine for when Cloud Shell is unavailable (for example a
# Google Workspace account that is not yet eligible for Cloud Shell).
#
# Create the VM in the console (Compute Engine → Create instance):
#   name devbox · region us-central1 · e2-standard-2 · Debian 12 · 30 GB balanced disk
# then open it with the SSH button and run:
#   curl -fsSL https://raw.githubusercontent.com/<owner>/<repo>/main/scripts/devbox-setup.sh | bash
# or clone the repo first and run: bash scripts/devbox-setup.sh
#
# Cost: ~$0.07/hour while running. Stop it when you are done
# (Compute Engine → devbox → Stop); a stopped VM only bills its disk.
set -euo pipefail

GO_VERSION="${GO_VERSION:-1.25.4}"
GOLANGCI_LINT_VERSION="${GOLANGCI_LINT_VERSION:-v2.5.0}"

sudo apt-get update
sudo apt-get install -y --no-install-recommends \
  ca-certificates curl git gnupg lsb-release make postgresql-client

# Docker Engine (for compose and testcontainers).
if ! command -v docker >/dev/null; then
  curl -fsSL https://get.docker.com | sudo sh
  sudo usermod -aG docker "$USER"
fi

# Go.
if ! /usr/local/go/bin/go version 2>/dev/null | grep -q "go${GO_VERSION} "; then
  curl -fsSL "https://go.dev/dl/go${GO_VERSION}.linux-amd64.tar.gz" -o /tmp/go.tgz
  sudo rm -rf /usr/local/go && sudo tar -C /usr/local -xzf /tmp/go.tgz && rm /tmp/go.tgz
fi
grep -q '/usr/local/go/bin' ~/.bashrc || echo 'export PATH=/usr/local/go/bin:$HOME/go/bin:$PATH' >> ~/.bashrc

# golangci-lint.
curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/HEAD/install.sh \
  | sudo sh -s -- -b /usr/local/bin "${GOLANGCI_LINT_VERSION}"

# Terraform.
if ! command -v terraform >/dev/null; then
  curl -fsSL https://apt.releases.hashicorp.com/gpg | sudo gpg --dearmor -o /usr/share/keyrings/hashicorp.gpg
  echo "deb [signed-by=/usr/share/keyrings/hashicorp.gpg] https://apt.releases.hashicorp.com $(lsb_release -cs) main" \
    | sudo tee /etc/apt/sources.list.d/hashicorp.list >/dev/null
  sudo apt-get update && sudo apt-get install -y terraform
fi

echo
echo "Done. Log out and back in (docker group), then:"
echo "  gcloud auth login --no-launch-browser"
echo "  gcloud auth application-default login --no-launch-browser   # for Terraform"
