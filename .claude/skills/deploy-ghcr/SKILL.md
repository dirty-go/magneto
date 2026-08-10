---
name: deploy-ghcr
description: Build and push the magneto Docker image to GitHub Container Registry (ghcr.io) via lib/ghcr-push.sh. Only run when the user explicitly asks to deploy/push/publish the image — never run automatically.
disable-model-invocation: true
---

Runs `lib/ghcr-push.sh`, which builds the Docker image and pushes it to
`ghcr.io/${OWNER:-iamucil}/${REPOSITORY:-magneto}`, tagged with the current
commit SHA and `latest`.

Before running, verify:

1. **Working tree is clean.** The script itself aborts on `git diff --stat`
   being non-empty (checks tracked-file changes only, not untracked files)
   — but check `git status` first and surface anything uncommitted to the
   user rather than letting the script fail.
2. **Docker daemon is running.** The script checks OrbStack/Docker Desktop/
   native socket paths and exits if none respond.
3. **`GITHUB_TOKEN` is set** in the environment, matching `^ghp_[a-zA-Z0-9_]{36,}$`
   (a classic PAT with `write:packages` scope). If unset or malformed, tell
   the user to `export GITHUB_TOKEN=ghp_...` before retrying — do not ask
   them to paste the token into chat.

Then run:

```
sh lib/ghcr-push.sh
```

Optional env overrides: `OWNER` (default `iamucil`), `REPOSITORY` (default
`magneto`), `GITHUB_USERNAME` (defaults to `OWNER`).

Note: the script also parses `-u -p -n -o -v` flags (gitlab userid/PAT,
node version, save dir, go variant) that are dead leftovers unrelated to
this repo's actual build — don't pass them.
