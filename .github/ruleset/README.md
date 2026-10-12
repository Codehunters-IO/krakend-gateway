# Rulesets

These files are definitions, not state: editing one changes nothing until it is sent to the API.
Both are repository-level rulesets, the only kind the organisation's plan allows; the org-level
definitions in `Codehunters-IO/ci-templates/.github/ruleset/` cannot be applied yet.

| File | Applies to | In force |
|------|-----------|----------|
| `ruleset-krakend-gateway-main.json` | `main` | ruleset `24900494` |
| `ruleset-krakend-gateway-tags.json` | release tags `vX.Y.Z` | ruleset `24900496` |

**`main`** takes changes only through a pull request, squash-merged, with signed commits and
these checks green on the latest base:

- `validate / Build / Build & Validate`, `validate / Test & Audit / Test & Audit`,
  `validate / Commit Lint / Lint Commit Messages` (the shared KrakenD pipeline)
- `Gateway config check`, `Plugin tests`, `Security headers smoke test` (`pull-request.yml`)

It cannot be deleted or force-pushed. Repository admins may merge a pull request without the
checks (`bypass_mode: pull_request`), never push directly.

**Release tags** cannot be moved or deleted once created. Creating them stays open: the
`Release to Production` workflow (`tag-pipeline.yml`) cuts `vX.Y.Z` with the workflow token.

Renaming a job in `pull-request.yml` or in the shared pipeline changes its check name: update
the list here and in the live ruleset in the same change, or every pull request waits on a
check that never reports.

Apply or update:

```bash
gh api -X POST repos/Codehunters-IO/krakend-gateway/rulesets --input .github/ruleset/<file>.json
gh api -X PUT  repos/Codehunters-IO/krakend-gateway/rulesets/<id> --input .github/ruleset/<file>.json
```

Check what is in force rather than trusting this table:

```bash
gh api repos/Codehunters-IO/krakend-gateway/rules/branches/main --jq '[.[].type]|unique'
```
