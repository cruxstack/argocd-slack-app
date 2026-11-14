# Agent Guidelines

## Architecture Overview
- **Deployment**: AWS Lambda (`provided.al2023`), deployed with terraform-aws-argocd-slack-app
- **Inputs**: Argo CD Applications, pods and events read through the EKS API; Slack interactivity, slash commands and events through an API Gateway HTTP API
- **State**: DynamoDB, one item per app per cluster plus cluster meta, mutes, a poll lease and once-only markers
- **Entry point**: `cmd/lambda/main.go` - loads config and SSM secrets, wires DynamoDB, Slack, EKS access and async self-invocation
- **Packages**:
  - `internal/argocdslack/` - the application
    - `config.go` - `ARGOCD_SLACK_CONFIG`, defaults, link and SCM templates
    - `snapshot.go` - Application JSON to `Snapshot`
    - `machine.go` - deploy and incident state machines (`Advance`); pure, no I/O
    - `reason.go`, `redact.go` - failure reasons from pods, events, HPAs and jobs; log line extraction and redaction
    - `poller.go`, `send.go`, `baseline.go` - list and watch per cluster, Slack sync, baseline summary
    - `render.go` - Block Kit payloads and views
    - `handler.go`, `mute.go`, `digest.go` - API Gateway routes, jobs, mutes, digest and App Home
    - `slack.go`, `store.go` - Slack Web API client and signature checks; DynamoDB store
    - `setup.go`, `manifest.go` - web installer (`/setup`, OAuth install and callback) and the generated app manifest
  - `internal/kube/` - minimal read-only Kubernetes client with EKS tokens
- **Tests**: beside the code; `fakes_test.go` holds the in-memory store, Slack and cluster; `testdata/` holds synthetic fixtures

## Build & Test
- **List targets**: `make help`
- **Build Lambda**: `make build-lambda` (creates `dist/bootstrap`)
- **Test all**: `make test` (runs with `-race -count=1`)
- **Test one function**: `go test -race -count=1 ./internal/argocdslack -run TestFlapReopensSameIncident`
- **Lint**: `make lint` (golangci-lint), `make fmt`
- **Vulnerabilities**: `make vuln` (govulncheck)
- **Everything CI runs**: `make check`

`make test-live` reads a real cluster through `kubectl proxy`; only run it when asked.

## Code Style
- **Imports**: stdlib, blank line, third-party, blank line, local (`github.com/cruxstack/argocd-slack-app/...`); `make fmt` enforces it
- **Comments**: lowercase inline comments; doc comments are full sentences
- **Logging**: `log/slog` with key-value pairs and snake_case keys
- **Message copy**: plain sentences people say out loud; the first line of every message is status · app · cluster

## Rules
- Keep the state machines free of I/O; enrichment and Slack calls happen in the poller
- Never post a new top-level message for a change to an existing deploy or incident; edit it and add a thread reply
- Only "I'm on it" writes app records outside the poller; anything else goes through the poller's tick
- The installer never replaces credentials once a bot token exists and never accepts a different workspace
- Test fixtures are synthetic; never commit real cluster output
