# Configuration

`ARGOCD_SLACK_CONFIG` is one JSON object. Every field is optional except
`clusters`, and each enabled cluster needs a channel (its own or the default).
Invalid config stops the function at startup with every problem listed.

## Top level

| Field              | Default                | Meaning                                                                                                                          |
| ------------------ | ---------------------- | -------------------------------------------------------------------------------------------------------------------------------- |
| `channel`          | `""`                   | Default Slack channel ID for clusters without their own.                                                                         |
| `channel_override` | `""`                   | Send every cluster to this channel, for example while testing.                                                                   |
| `dry_run`          | `false`                | Log every Slack call instead of making it.                                                                                       |
| `timezone`         | `UTC`                  | Zone for the digest's date. Message times use Slack date tokens, so each reader sees their own.                                  |
| `watch_seconds`    | `55`                   | How long each run watches before the next one starts (1–58).                                                                     |
| `rich_blocks`      | `true`                 | Frame statuses in `container` blocks and the digest in a `data_table`. The app also falls back on its own if Slack refuses them. |
| `icon_base_url`    | this release on GitHub | Where Slack loads the status icons (`ok.png`, `fail.png`, `warn.png`, `run.png`, `muted.png`); `none` turns them off.            |
| `preview_prefix`   | `preview-`             | Apps with this prefix are flagged as cleanup candidates in the digest.                                                           |
| `owners`           | `{}`                   | App name to a Slack user group ID (`S…`) or user ID (`U…`), mentioned on `prod` incidents and failed deploys.                    |
| `annotations`      | all `""`               | Application annotation names for deploy metadata; see below.                                                                     |
| `scm`              | GitHub                 | Link templates; see below.                                                                                                       |
| `links`            | `[]`                   | Links on every message; see below.                                                                                               |
| `grace`            | see below              | Timing rules.                                                                                                                    |
| `clusters`         | required               | The Argo CD instances to watch.                                                                                                  |
| `setup`            | off                    | The web installer; see below.                                                                                                    |

## Timing rules (`grace`)

Durations accept Go syntax (`90s`, `5m`, `1h`) plus `d` and `w`.

| Field             | Default | Rule                                                                  |
| ----------------- | ------- | --------------------------------------------------------------------- |
| `incident`        | `5m`    | How long an app stays unhealthy before an incident opens.             |
| `autoscaler`      | `10m`   | The same, when the only culprit is a HorizontalPodAutoscaler.         |
| `flap`            | `15m`   | Unhealthy again within this time reopens the same message.            |
| `deploy_window`   | `10m`   | Degradation this soon after a deploy belongs to the deploy's message. |
| `stall`           | `15m`   | A rollout still progressing after this is shown as stalled.           |
| `long_running`    | `24h`   | Incidents older than this go in the digest.                           |
| `preview_cleanup` | `72h`   | Previews unhealthy this long are flagged as cleanup candidates.       |
| `annotation_lag`  | `5m`    | Annotations changed this soon after a sync started still describe it. |

## Deploy metadata (`annotations`)

Name the Application annotations your pipeline writes. Every one is optional;
a message shows what it has and leaves the rest out.

```json
"annotations": {
  "repo": "example.com/repo",
  "commit": "example.com/commit",
  "version": "example.com/version",
  "actor": "example.com/actor",
  "approved_by": "example.com/approved-by",
  "pr": "example.com/pr",
  "title": "example.com/commit-title",
  "run_url": "example.com/run-url",
  "changes": "example.com/changes"
}
```

| Name          | Shown as                                                                                     |
| ------------- | -------------------------------------------------------------------------------------------- |
| `repo`        | Source repo, `owner/name` or a Git URL, for commit, PR, version and compare links.           |
| `commit`      | Source commit.                                                                               |
| `version`     | Release in the heading, such as `api v1.5.0 deployed to prod-use1`.                          |
| `actor`       | Who started the deploy: `by mara-lee`.                                                       |
| `approved_by` | Who approved it: `started and approved by mara-lee`, or `by mara-lee, approved by jun-park`. |
| `pr`, `title` | A single change's title under the line, when there is no `changes` list.                     |
| `run_url`     | The `Deployment` link, first in the grey line.                                               |
| `changes`     | A thread reply listing the changes, up to ten.                                               |

The line under the heading reads like `v1.4.0 → v1.5.0 · started and approved
by mara-lee · 1m 15s`. The app remembers what each app last deployed, so the
range needs nothing from the pipeline: versions when both deploys have one,
short commits otherwise, linked to `scm.compare_url`.

`changes` is a JSON array, plain or base64-encoded so quotes survive YAML
templating:

```json
[
  {
    "id": "#482",
    "title": "feat: one-tap checkout",
    "url": "https://github.com/acme/checkout/pull/482",
    "author": "mara-lee"
  }
]
```

Only `title` is required. When every `id` starts with `#` the reply says
"PRs", otherwise "changes".

Unset names are skipped. Without `repo` and `commit`, the message links the
Application's own source repo at its sync revision. Values that still contain
an unrendered `{{placeholder}}` count as empty.

## Link templates (`scm`)

| Field         | Default                                           | Placeholders               |
| ------------- | ------------------------------------------------- | -------------------------- |
| `commit_url`  | `https://github.com/{repo}/commit/{commit}`       | `{repo}`, `{commit}`       |
| `pr_url`      | `https://github.com/{repo}/pull/{pr}`             | `{repo}`, `{pr}`           |
| `compare_url` | `https://github.com/{repo}/compare/{from}...{to}` | `{repo}`, `{from}`, `{to}` |
| `version_url` | off                                               | `{repo}`, `{version}`      |

Set `version_url` to link versions, for example to
`https://github.com/{repo}/releases/tag/{version}`.

## Links

`links` (global) and each cluster's `links` add up to three links, cluster
links first. URLs are templates; each `{name}` is replaced, URL-escaped, with
one of `app`, `namespace` (the destination namespace), `cluster`, `env`,
`region`, or a key from the cluster's `vars`:

```json
"links": [
  {"text": "Logs", "url": "https://grafana.example.com/explore?var-cluster={telemetry_cluster}&var-namespace={namespace}"},
  {"text": "Metrics", "url": "https://grafana.example.com/d/workloads?var-namespace={namespace}"}
],
"clusters": [
  {"name": "prod-use1", "cluster": "prod-cluster", "vars": {"telemetry_cluster": "prod-main"}}
]
```

Prefer global links with per-cluster `vars`: Lambda environment variables are
limited to 4 KB in total.

## Clusters

| Field        | Default     | Meaning                                                                  |
| ------------ | ----------- | ------------------------------------------------------------------------ |
| `name`       | required    | Short name shown on messages and used in mutes, such as `prod-use1`.     |
| `cluster`    | required    | EKS cluster name.                                                        |
| `env`        | `""`        | Environment, such as `dev` or `prod`; mutes can target it.               |
| `region`     | `us-east-1` | The cluster's AWS region.                                                |
| `role_arn`   | `""`        | Role to assume for the cluster; empty uses the function's own role.      |
| `namespace`  | `argocd`    | Namespace of the Argo CD Applications.                                   |
| `argocd_url` | `""`        | Argo CD web URL, for the Argo CD link.                                   |
| `channel`    | `""`        | Slack channel ID; falls back to the top-level `channel`.                 |
| `enabled`    | `false`     | Watch this cluster.                                                      |
| `log_lines`  | `false`     | Show one redacted error line from a crashed container. Needs `pods/log`. |
| `links`      | `[]`        | Extra links for this cluster.                                            |
| `vars`       | `{}`        | Values for link templates.                                               |

When a cluster is enabled it posts one baseline message listing apps that are
already unhealthy, instead of one alert each. Moving it to another channel, or
leaving dry run, posts a fresh baseline.

## Installer (`setup`)

| Field      | Default   | Meaning                                                                       |
| ---------- | --------- | ----------------------------------------------------------------------------- |
| `enabled`  | `false`   | Serve `/setup` and the OAuth install. Turn it on for the first deploy only.   |
| `app_name` | `Argo CD` | The Slack app's and bot's display name in the manifest.                       |
| `command`  | `/argocd` | The slash command in the manifest.                                            |
| `base_url` | request   | Public base URL for the manifest's request URLs, if not the API Gateway host. |

The installer writes `SLACK_SIGNING_SECRET`, `SLACK_CLIENT_ID`,
`SLACK_CLIENT_SECRET`, `SLACK_BOT_TOKEN` and `SLACK_TEAM` to the SSM parameters
those environment variables name, so the function needs `ssm:PutParameter` on
them while it's on.
