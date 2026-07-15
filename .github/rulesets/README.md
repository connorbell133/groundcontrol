# Repository rulesets

GitHub does not read rulesets from this folder automatically — these JSON files
are the version-controlled source of truth, applied to the repo via the API or
imported through the UI (Settings → Rules → Rulesets → New ruleset → Import).

## Rulesets

- **protect-main.json** — the default branch (`main`) can only change via pull
  request, the CI `check` job must pass, review threads must be resolved, and
  the branch can't be force-pushed or deleted. Zero required approvals so a solo
  maintainer can merge their own PRs. Repository admins bypass everything.
- **protect-release-tags.json** — `v*` tags can't be deleted, moved, or
  force-updated once pushed. Repository admins bypass.

## Apply / update

```sh
# create
gh api repos/{owner}/{repo}/rulesets --input .github/rulesets/protect-main.json

# list existing (to get ids)
gh api repos/{owner}/{repo}/rulesets

# update in place
gh api -X PUT repos/{owner}/{repo}/rulesets/<id> --input .github/rulesets/protect-main.json
```

After editing a JSON file here, re-apply it with the PUT command so the repo
setting stays in sync with the file.
