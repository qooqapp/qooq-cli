# qooq-cli

CLI for qooq — talks to `qooqd` (orchestrator daemon) and `qooq-watcher`
(GitOps service) over their REST APIs.

## Install

### Installing the binaries

```bash
curl -fsSL https://raw.githubusercontent.com/qooqapp/qooq/main/install.sh | sh

# ~/.local/bin needs no sudo. Drop PREFIX for the default, /usr/local/bin.
curl -fsSL https://raw.githubusercontent.com/qooqapp/qooq/main/install.sh | PREFIX=$HOME/.local/bin sh
```

One installer, three binaries: `qooqd`, `qooq-watcher`, `qooq`. It verifies the
published checksums and prints `verified` for each archive — a run that skips
that line downloaded something else.

```bash
export PATH="$HOME/.local/bin:$PATH"   # the installer prints this line if needed
qooq --version                         # -> qooq version 0.7.0
```

`dev` or an empty version means the binary was not built by a release: check
which `qooq` is first on `PATH`.

Then, with docker running:

```bash
qooq up       # qooqd + qooq-watcher in the background, state under ~/.qooq
qooq status   # both should report healthy
```

`VERSION` is deliberately left out above. `install.sh` applies it to *both*
repositories, so pinning it only works while the two carry the same tag;
omitted, each resolves its own latest release.

### Installing the Claude Code skill

`qooq-deploy` detects a project's type, writes its `qooq.yaml`, deploys it, and
curls the result before claiming it worked.

**With the CLI already installed** — needs no GitHub access at all:

```bash
qooq skill install qooq-deploy   # -> ~/.claude/skills/qooq-deploy
qooq skill ls                    # INSTALLED should read "yes"
```

**From Claude Code, without installing anything first:**

```
/plugin marketplace add qooqapp/qooq
/plugin install qooq-deploy@qooq
```

The skill installs qooq itself when the machine does not have it, using the same
`install.sh`.

Either way, **start a new Claude Code session** — a skill is loaded at startup.
Then, in a project directory, say "deploy this" and expect a URL that opens.

### Installing without GitHub

`install.sh` reads two knobs that skip the GitHub API: `QOOQ_DOWNLOAD_BASE` for
a mirror serving the release archives (needs an explicit `VERSION`, since a
plain directory has no "latest" to ask for), and `QOOQ_API_BASE` for a GitHub
Enterprise host. Both are covered by `make install-test` in the qooq repository.

## Build from source

This module's `go.mod` has `replace qooq => ../qooq`, so the
[qooq](https://github.com/qooqapp/qooq) repo must be checked out side-by-side
at `../qooq`:

```bash
git clone https://github.com/qooqapp/qooq ../qooq   # if not already there
make build                                          # -> bin/qooq
```

## Usage

```
qooq [global flags] <command> [subcommand] [args] [flags]
```

### Global flags

| Flag | Env | Default | Description |
|---|---|---|---|
| `--addr` | `QOOQ_ADDR` | `http://127.0.0.1:7777` | qooqd base URL |
| `--token` | `QOOQ_TOKEN` | *(none)* | bearer token for the qooqd API |

The `watch` command tree has its own pair of flags (see below), since it talks
to a separate daemon.

### `qooq system` — daemon health and version

| Command | Args | Flags | Description |
|---|---|---|---|
| `system health` | — | — | check daemon health |
| `system version` | — | — | show daemon version |
| `system backup <output-file>` | output file | — | write a consistent datastore snapshot to a file (mode 0600) |

### `qooq component` (alias `comp`) — manage components

| Command | Args | Description |
|---|---|---|
| `component deploy` | — | deploy a component from an image ref, tarball, or Dockerfile build |
| `component upsert` | — | create or redeploy a component by name (idempotent) |
| `component ls` (alias `list`) | — | list components |
| `component get <id-or-name>` | id/name | show a component |
| `component start <id-or-name>` | id/name | start a component |
| `component stop <id-or-name>` | id/name | stop a component |
| `component rollback <id-or-name>` | id/name | roll back to the previous image (run again to undo) |
| `component rm <id-or-name>` (alias `remove`) | id/name | remove a component |
| `component stats <id-or-name>` | id/name | show latest resource usage; `-w`/`--watch` streams a live dashboard |
| `component events <id-or-name>` | id/name | list lifecycle/crash events |
| `component logs <id-or-name>` | id/name | show container logs; `-f`/`--follow` to stream, `--tail N` for the last N lines |

`deploy` and `upsert` share the same set of flags:

| Flag | Description |
|---|---|
| `--name` | component name (**required**) |
| `--image` | image ref, e.g. `nginx:alpine` |
| `--tar` | path to a `docker save` tarball to load and deploy |
| `--build` | directory with a Dockerfile to build and deploy |
| `--dockerfile` | Dockerfile path relative to `--build` (default: `Dockerfile`) |
| `--tag` | tag to apply when building with `--build` |
| `--cmd` | override container command, one arg per flag (repeatable) |
| `--env` | environment variable `KEY=VALUE` (repeatable) |
| `--port` | port mapping `[HOST:]CONTAINER[/proto]` (repeatable) |
| `--group` | group name (auto-created if missing) |
| `--restart` | restart policy: `no` (default), `on-failure`, or `always` |
| `--cpu` | CPU limit in cores, e.g. `0.5` |
| `--mem` | memory limit, e.g. `64m`, `1g` |
| `--health-http` | HTTP health check path, e.g. `/healthz` (needs `--health-port`; mutually exclusive with `--health-exec`) |
| `--health-port` | HTTP health check published host port |
| `--health-exec` | exec health check command, one arg per flag (repeatable; mutually exclusive with `--health-http`) |
| `--health-interval` | probe interval in seconds (default 10) |
| `--health-timeout` | probe timeout in seconds (default 5) |
| `--health-failures` | consecutive probe failures before restart (default 3) |
| `--health-delay` | delay before the first probe, in seconds (default 5) |

Exactly one of `--image` / `--tar` / `--build` must be set.

### `qooq group` (alias `grp`) — manage groups

| Command | Args | Flags | Description |
|---|---|---|---|
| `group create <name>` | name | — | create a group |
| `group ls` (alias `list`) | — | — | list groups |
| `group get <id-or-name>` | id/name | — | show a group and its members |
| `group add <group> <component>` | group id/name, component id/name | — | add a component to a group (group auto-created if missing) |
| `group rm-member <group> <component>` | group id/name, component id/name | — | remove a component from a group |
| `group start <id-or-name>` | id/name | — | start all members of a group |
| `group stop <id-or-name>` | id/name | — | stop all members of a group |
| `group rm <id-or-name>` (alias `remove`) | id/name | `--cascade` | delete a group; `--cascade` also removes its components |

### `qooq image` — manage images

| Command | Args | Flags | Description |
|---|---|---|---|
| `image pull <ref>` | image ref | `--platform` (e.g. `linux/arm64`) | pull an image from a registry |
| `image load <tarball>` | tarball path | — | load a `docker save` tarball |
| `image build <dir>` | build context dir | `--dockerfile`, `--tag` | build an image from a Dockerfile |

### `qooq watch` — manage qooq-watcher GitOps targets

| Flag | Env | Default | Description |
|---|---|---|---|
| `--watcher-addr` | `QOOQ_WATCHER_ADDR` | `http://127.0.0.1:7778` | qooq-watcher base URL |
| `--watcher-token` | `QOOQ_WATCHER_TOKEN` | *(none)* | bearer token for the qooq-watcher API |

| Command | Args | Description |
|---|---|---|
| `watch add` | — | register a new watch target (GitOps `source` mode or registry-digest `image` mode) |
| `watch ls` (alias `list`) | — | list watch targets (MODE column shows source/image) |
| `watch get <name>` | name | show a watch target |
| `watch rm <name>` (alias `remove`) | name | remove a watch target |
| `watch trigger <name>` | name | check now, bypassing the poll interval; `--force` rebuilds even when the remote has not changed |
| `watch redeploy --stale-addr` | — | rebuild every target whose component still names an address this machine has lost; `-n/--dry-run` to list them first |
| `watch deploy <name> --image REF` | name | deploy a specific image to the target's component right now (M20b, for CI pipelines); for `image` targets the deployed ref becomes the new polling baseline |
| `watch backup <output-file>` | output file | write a consistent watcher DB snapshot to a file (mode 0600; contains credentials) |

`watch add` flags:

| Flag | Description |
|---|---|
| `--name` | target name (**required**) |
| `--deploy-mode` | `source` (default: clone + build the repo) or `image` (M20a: poll a registry tag's digest and deploy it on change — no clone, no build) |
| `--image-ref` | image to poll, e.g. `ghcr.io/you/web:latest` (**required in `image` mode**; rejected in `source` mode) |
| `--repo` | git repository URL (**required in `source` mode**; rejected in `image` mode) |
| `--branch` | branch to watch (default: `main`; `source` mode only) |
| `--auth-token` | in `source` mode: HTTPS auth token; in `image` mode: registry credential as `username:password`. Either way `@file` reads it from a file (mutually exclusive with `--auth-ssh-key`) |
| `--auth-ssh-key` | path to a PEM-encoded SSH private key file (mutually exclusive with `--auth-token`) |
| `--dockerfile` | Dockerfile path relative to the build context (default: `Dockerfile`) |
| `--context-subdir` | build context subdirectory within the repo |
| `--manifest` | repo-relative path to the app manifest (default: `qooq.yaml`; `-` disables manifest parsing) |
| `--component` | qooqd component name to build and deploy (**required**) |
| `--poll` | poll interval in seconds (default: 60, minimum: 10) |
| `--webhook-secret` | shared secret for webhook signature verification, or `@file` (unset disables webhooks for this target) |
| `--deploy-spec` | deploy spec as a JSON string or `@file` (command/env/ports/group_id/restart_policy/cpu_limit/mem_limit_bytes/health_check) |

`--auth-token`, `--webhook-secret`, and `--deploy-spec` all accept an `@path/to/file`
value to read the content from a file instead of the shell.

### `qooq stack` — deploy a group of projects together

One `qooq.stack.yaml` in a git repository names several projects, says which
waits for which, and deploys them into a namespace and network named after the
stack. Each project keeps its own `qooq.yaml`: the stack manifest decides *when*
each one is built, the ordinary pipeline still decides *how*.

| Command | Flags | Description |
|---|---|---|
| `stack add` | `--name --repo [--branch] [--manifest] [--auth-token\|--auth-ssh-key] [--poll]` | register a stack from the repo holding its `qooq.stack.yaml` |
| `stack ls` | — | every stack, with its status and last sync |
| `stack get <name>` | — | the stack plus a row per project: what it waits for, its build status, its failure count |
| `stack manifest <name>` | — | the manifest the watcher is working from (the last one that parsed) |
| `stack trigger <name>` | — | deploy now, bypassing the poll interval |
| `stack resume <name>` | — | lift a pause and give every project a fresh failure budget |
| `stack rm <name>` | `--prune` | forget the stack; the components keep running unless `--prune` |

```bash
bin/qooq stack add --name acme-prod --repo https://github.com/acme/prod.git
bin/qooq stack get acme-prod
```

A stack goes `degraded` when some projects deployed and others did not (it is
running a mix of versions), and `paused` when one project has failed
`policy.max_failures` times in a row — at which point nothing is deployed until
you resume, though everything already running keeps running.

### `qooq init` — write a qooq.yaml for what is in front of you

Looks at a project — its Dockerfile, its package.json, what it exposes — and
writes a `qooq.yaml` describing how to build and run it. Like `manifest` and
`skill`, it never talks to a daemon: writing a manifest is something you do
before there is anything to talk to.

| Command | Args | Flags | Description |
|---|---|---|---|
| `init [dir]` | directory (default `.`) | `--app`, `--force`, `-n/--dry-run` | write `qooq.yaml`; refuses to replace an existing one without `--force` |

```bash
qooq init            # writes ./qooq.yaml
qooq init -n         # print it instead, to see what it would say
```

The result is a draft to read, not an answer. Every guess is written into the
file as a comment, so you can see what each line was based on — and the health
check is left commented out with the paths worth trying, because which one your
app serves is in its code, not in its files, and a guess there produces a probe
that fails forever against an app that is perfectly fine.

It runs the same detection the watcher runs on a repo that arrives without a
manifest, so what you get here and what qooq would have generated cannot drift
apart.

### `qooq manifest` — check a qooq.yaml before you push it

Validates an app manifest locally. Like `skill`, it never talks to a daemon —
requiring one would put the check on the far side of the loop it exists to
shorten. A manifest mistake otherwise surfaces only after commit, push, poll,
clone and build, and the error arrives somewhere its author is no longer
looking.

| Command | Args | Description |
|---|---|---|
| `manifest validate [file]` | path (default `./qooq.yaml`, `-` for stdin) | parse and validate; exits non-zero when the manifest would fail to deploy |

```bash
qooq manifest validate                  # ./qooq.yaml
qooq manifest validate api/qooq.yaml
```

A passing check also prints what the manifest asks for — the build, the ports,
the services, the required inputs — because "valid" and "says what I meant" are
different questions, and a mistyped key silently gets the second one wrong.

### `qooq skill` — install the bundled Claude Code skills

The skills ship inside the binary, so this is the one command tree that never
talks to a daemon: it works before qooqd exists.

| Command | Args | Flags | Description |
|---|---|---|---|
| `skill ls` | — | — | bundled skills and whether each is installed |
| `skill install [name]` | skill name (default: all) | `--dir` (default `~/.claude/skills`), `--force` | install a skill; without `--force` it refuses to overwrite an existing one |
| `mcp` | — | — | run the MCP server on stdin/stdout (agents start it; tools `qooq_deploy`, `qooq_list`, `qooq_logs`, `qooq_build_log`, `qooq_secret_*`, …) |
| `mcp install` | — | `--agent claude\|cursor\|codex` (default claude) | register `qooq mcp` with the agent, by the binary's absolute path |

A skill is read at startup, so start a new Claude Code session after installing.

### `qooq service` — start the daemons on boot

### Settings: `qooq config`

`qooq up` and `qooq service install` read `~/.qooq/config.yaml`:

| Command | Effect |
|---|---|
| `qooq config` | show the current settings |
| `qooq config set ingress.port <n>` | port apps get `http://<name>.localhost` on (default 80; `0` = off, apps at IP:port) |
| `qooq config set access local\|lan` | `lan` (default): the local network can open apps too; `local`: this machine only |
| `qooq config unset <key>` | back to the default |

Changes apply the next time qooq starts (`qooq down && qooq up`). If the ingress port is taken
by another program, `qooq up` says so and falls back to IP:port for that run.

`qooq up` lasts until you log out. On a machine meant to keep running, install
the daemons as a user service instead: launchd on macOS, `systemd --user` on
Linux. The unit is generated from the same daemon definitions `qooq up` uses,
so there is only ever one description of how qooqd is started.

| Command | Flags | Description |
|---|---|---|
| `service install` | same as `up` (`--ingress`, `--ingress-tls`, `--ingress-tls-issuer`, `--no-watcher`, `--qooqd-arg`) | write the unit files and start them; refuses if a daemon is already running |
| `service uninstall` | — | stop the services and delete the units; data and containers untouched |
| `service status` | — | installed / healthy per daemon |

```bash
qooq down && qooq service install
```

Two things it handles that a hand-written unit usually does not: the docker
endpoint (OrbStack, Colima and Rancher Desktop record a non-standard socket in
a docker *context*, which the daemons do not read — so it is resolved at
install time and written into the unit, meaning **docker should be running when
you install**), and the boot race (qooqd exits when docker is unreachable, and
docker is usually a few seconds behind it, so both unit types retry every 10s).

On a Linux server you reach over SSH, user services stop at logout unless you
run `loginctl enable-linger $USER`.

### `qooq compose` — start/stop the whole daemon pair

Wraps `docker compose` over the compose files in `--dir` (default:
`QOOQ_COMPOSE_DIR` env, else the current directory) — `docker-compose.yml`
plus `docker-compose.e2e.yml` when present. Compose env (`QOOQ_TOKEN`,
`QOOQ_WATCHER_SECRET_KEY`, …) comes from your shell or a `.env` file in that
directory.

> Renamed from `qooq stack` on 2026-07-28, since `stack` now means a group of
> deployed projects (above). `QOOQ_STACK_DIR` is still read as a fallback.

| Command | Flags | Description |
|---|---|---|
| `compose up` | `--build` | start qooqd + qooq-watcher and wait until healthy; `--build` rebuilds images first |
| `compose stop` | — | stop the containers, keeping them and their data |
| `compose down` | `-v/--volumes` | remove the containers; `-v` **also deletes the data volumes** (irreversible) |
| `compose status` | — | show services and health |

```bash
export QOOQ_COMPOSE_DIR=~/projects/qooqapp/qooq   # set once (or use --dir)
bin/qooq compose up
bin/qooq compose status
bin/qooq compose stop
```

### Examples

```bash
bin/qooq system health
bin/qooq --token s3cret component ls
bin/qooq component deploy --name web --image nginx:alpine --port 8080:80
bin/qooq component logs web -f --tail 100
bin/qooq watch add --name demo --repo https://github.com/acme/demo.git \
  --component demo-web --auth-token @./token.txt
bin/qooq watch add --name webprod --deploy-mode image \
  --image-ref ghcr.io/acme/web:latest --component web --poll 60
bin/qooq watch deploy webprod --image ghcr.io/acme/web:sha-abc123
bin/qooq watch ls
```

Full walkthrough (deploy, GitOps, webhook, backup): `../qooq/docs/USAGE.md`.

## Checks

```bash
make check   # vet + dependency wall: the CLI must not pull in gin/fx/gorm/go-git/docker SDK
```
