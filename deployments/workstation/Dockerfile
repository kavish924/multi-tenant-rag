# Cloud Workstations image: Google's Code OSS base (includes Docker and gcloud)
# plus the project toolchain. Built by deployments/workstation/cloudbuild.yaml
# into Artifact Registry; the workstation config (terraform/bootstrap) uses it.
#
# Bump versions here and rebuild; never install tools by hand on a workstation.

FROM us-central1-docker.pkg.dev/cloud-workstations-images/predefined/code-oss:latest

ARG GO_VERSION=1.25.4
ARG GOLANGCI_LINT_VERSION=v2.5.0

USER root

# Go (pinned; matches the go directive in go.mod and the Dockerfile builder).
RUN curl -fsSL "https://go.dev/dl/go${GO_VERSION}.linux-amd64.tar.gz" -o /tmp/go.tgz \
 && rm -rf /usr/local/go && tar -C /usr/local -xzf /tmp/go.tgz && rm /tmp/go.tgz
ENV PATH=/usr/local/go/bin:$PATH

# golangci-lint (pinned), installed by its release script into /usr/local/bin.
RUN curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/HEAD/install.sh \
    | sh -s -- -b /usr/local/bin "${GOLANGCI_LINT_VERSION}"

# Terraform (HashiCorp apt repository), psql, make.
RUN apt-get update \
 && apt-get install -y --no-install-recommends gnupg lsb-release make postgresql-client \
 && curl -fsSL https://apt.releases.hashicorp.com/gpg | gpg --dearmor -o /usr/share/keyrings/hashicorp.gpg \
 && echo "deb [signed-by=/usr/share/keyrings/hashicorp.gpg] https://apt.releases.hashicorp.com $(lsb_release -cs) main" \
    > /etc/apt/sources.list.d/hashicorp.list \
 && apt-get update && apt-get install -y --no-install-recommends terraform \
 && rm -rf /var/lib/apt/lists/*

# Go paths for the workstation user land on the persistent home disk.
RUN echo 'export PATH=/usr/local/go/bin:$HOME/go/bin:$PATH' > /etc/profile.d/go.sh
