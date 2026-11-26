---
name: review-public-source
description: Review a public-source change, PR or release for leaked organizational information, infrastructure details and credentials before publication or merge. Use for public Sluicework publication reviews; preserve intentional public attribution and generic examples.
---

# Review public source

Review the exact candidate commit and its ancestry, not only the working diff.
Keep private deployment inventories, credentials and reviewer reference files
outside the public checkout. This skill does not authorize publishing, changing
access, rotating credentials or deploying infrastructure.

## Establish scope

Confirm the intended public repository, base/head commits and requested action.
Read the repository's contribution/configuration guidance. Identify which
organization-specific facts the change might expose. Public product ownership,
copyright notices and documented provider identifiers are not automatically
confidential; deployment-specific values and relationships require review even
when individual names are publicly discoverable.

## Inspect before pushing

Run the repository's publication checker on working files, the staged snapshot
and candidate history as appropriate. For Sluicework:

```sh
python3 scripts/check-public-source.py
python3 scripts/check-public-source.py --staged
python3 scripts/check-public-source.py --history
```

When a protected private reference file is available, repeat the relevant check
with `--private-reference /absolute/private/path/identifiers.txt`. The local
default location to check is `~/.config/sluicework/review/private-identifiers.txt`;
use it only if present and relevant to this repository. Each line is a literal
private identifier, not a regular expression. Never copy that file or its values
into source, fixtures, PR text, logs or a public CI secret. Public CI performs the
generic checks without access to an organization's inventory.

Inspect the diff and newly introduced historical files for repository inventories,
App/installation IDs, account IDs, ARNs, resource names, regions tied to a real
deployment, hostnames/IPs, private endpoints, runner labels, personal paths and
operator identities. Review workflows, examples, fixtures and installer defaults;
an ignore rule does not remove tracked data or older commits.

Inspect changed screenshots, archives, generated binaries, logs and exports
separately. Pattern checks cannot reliably inspect pixels, compressed content or
contextual organizational relationships. Review the proposed commit messages,
branch names, PR title/body and artifact names before publishing those too.

## Resolve and report

Use synthetic replacements without embedding real values in regression tests or
public denylists. Keep deployment configuration in a separate private companion
repository and secrets in protected runtime storage or a secret manager.
Recheck the exact staged/committed result after fixes. If sensitive material is
already published, report its category privately; do not claim deleting the tip
file removes history or perform unauthorized history rewrites/rotations.

Report findings by category and location without reproducing sensitive values.
State which source/history/artifacts were checked and any material limits.
Proceed with an already-authorized push/merge only after resolving findings and
the repository's required checks. Do not claim an independent approval when
reviewing your own implementation.
