# Path ownership: ConOps vs Puppet / config management

ConOps and Puppet must not fight over the same paths.

## Owned by ConOps (Puppet must not chmod/chown)

| Path | Mode | Notes |
|------|------|-------|
| `$CONOPS_RUNTIME_DIR` (default `./.conops-runtime` or chart value) | `0755` | App roots |
| `$CONOPS_RUNTIME_DIR/<app>/repo/**` (except `.git`) | `0755` / `0644` | Bind-mount source for containers |
| `$CONOPS_RUNTIME_DIR/<app>/repo/.git` | `0700` / `0600` | Private |
| `$CONOPS_RUNTIME_DIR/<app>/.ssh`, `.envs` | `0700` / `0600` | Deploy keys / overrides |
| `$CONOPS_DATA` / `/data/conops.db` | `0600` | SQLite |
| `/data/conops-encryption.key`, TLS, bootstrap token | `0600` | Secrets |
| `/data/fleet-cache` | `0700` | Fleet git cache |

ConOps only adjusts modes on paths **it creates** under the runtime/data dirs. It does not recurse into host paths outside those trees.

## Owned by Puppet / hiera-eyaml

| Path | Notes |
|------|-------|
| `/etc/conops/secrets.env` (or compose `env_file`) | `CONOPS_*_TOKEN`, API token |
| `/etc/tenko/apps/<app>/*.env` | App secrets bind-mounted into compose |
| Unit/compose that **runs** the ConOps container | Image tag, mounts, restart |
| Host docker / users | Not ConOps |

## Rule of thumb

- Puppet writes **secrets and unit config** on the host.
- ConOps writes **git checkouts and its DB** under runtime/data.
- Never `file { '/opt/conops/runtime': mode => ... }` from Puppet if ConOps manages that tree.
