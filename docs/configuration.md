# Public development and private configuration

Develop the application in the public repository. Keep an organization's
deployment configuration in a separate private repository or private directory,
alongside the public checkout rather than inside it. Do not merge private Git
history into the public repository, including when the final files look clean.

## Configuration ownership

| Information | Location |
| --- | --- |
| Application code, reusable integrations, synthetic examples | Public source |
| Runner policies, resources, provider connections, operational settings | Main Sluicework admin and its private database |
| Initial server/worker settings and trust paths | Installer-generated files outside the checkout |
| Repository allowlists and cloud infrastructure parameters | Private deployment configuration |
| GitHub/provider keys and TLS private keys | Protected runtime files or AWS Secrets Manager |
| Developer configuration paths and AWS profiles | Explicit, ignored environment file |

Keep the installed cluster and its data independent of the source checkout.
Changing development directories does not require resetting or restarting it.
Continue using the main admin for operational edits; environment files do not
override settings saved in the admin database.

## Optional environment file

Use Go and Node versions from the README, then build the console and binary:

```sh
(cd web && npm ci && npm run build)
go build -buildvcs=false -o sluicework ./cmd/sluicework
install -m 600 .env.example .env.local
```

Set `SLUICEWORK_CONFIG` to the absolute path of an existing external JSON file.
Select the environment file explicitly before the command:

```sh
./sluicework --env-file .env.local server
./sluicework --env-file .env.local backup --out /private/backup/new-snapshot
./sluicework --env-file .env.local worker --config /private/cluster/worker.json
```

`SLUICEWORK_CONFIG` supplies the default `--config` for server, worker,
workstation, queue and coding-client administration commands. Explicit
`--config` wins. Existing process variables win over environment-file values,
including variables deliberately exported empty. Use different files or explicit
paths for different roles. Personal agents and Codex client credentials retain
their separate, explicitly selected runtime/client files.

No `.env` file is loaded automatically, including when opening a coding workspace.
Supported syntax is single-line `NAME=value`, optional `export`, blank lines,
comments, literal single quotes and JSON-style double quotes. Shell commands,
variable expansion and multiline values are not evaluated. Use absolute paths;
`$HOME` and `~` in values are literal. Files must be private regular files
(`chmod 600`), not symlinks, and at most 64 KiB. Invalid-file errors omit values.

For Lambda deployments, supply `QUEUE_CONFIG`, `QUEUE_TABLE` and
`GITHUB_KEY_SECRET_ARN` through the reviewed infrastructure deployment. A local
environment file does not update Lambda. Never start a second dispatcher using
the same backend ID with an independent state store.

## Public changes and artifacts

Commit only `.env.example` and synthetic configuration examples. Ignore rules
cover local environment files, runtime JSON, private keys and local directories;
they do not protect a file already tracked or added with `git add -f`.

Run `python3 scripts/check-public-source.py` before committing and
`python3 scripts/check-public-source.py --history` before publishing. Public CI
checks both the committed tree and reachable history for common credentials,
private runtime files, deployment identifiers and personal home paths. The
checker reports locations and categories without printing matched values.
Review screenshots, logs, archives and build artifacts separately: pattern
checks cannot establish that every organization-specific detail is safe.

For a local review, optionally supply a mode-600 UTF-8 file outside the checkout,
with one literal private identifier per line (blank lines and `#` comment lines
are ignored). Include deployment hostnames, account/resource identifiers, private
repository names and machine labels that generic patterns cannot recognize:

```sh
python3 scripts/check-public-source.py --private-reference /private/review/identifiers.txt
python3 scripts/check-public-source.py --history --private-reference /private/review/identifiers.txt
```

Matches are case-insensitive and values are omitted from reports. Repeat
`--private-reference` for multiple organizations. Do not put real identifiers
into public fixtures or CI variables to teach the checker about an organization.
Use the [public-source review skill](../.agents/skills/review-public-source/SKILL.md)
to review both organization information and infrastructure details, including
PR text and artifacts that source patterns cannot establish are safe.

Public CI uses hosted Linux. Connecting organization-owned runners to public
source is a separate, reviewed trust decision; never let an untrusted PR select
private workers or deployment credentials.
