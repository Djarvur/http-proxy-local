---
title: "Periodic CI check failed"
labels: ["ci", "maintenance"]
---

The weekly periodic CI check failed. This is a non-blocking signal —
in-flight PRs are unaffected — but it indicates something needs attention.

## What to check

Review the latest run:
https://github.com/{{ env.GITHUB_REPOSITORY }}/actions/workflows/periodic.yml

Common causes:
- A dependency introduced a known vulnerability (govulncheck)
- A dependency updated and broke a test
- The latest golangci-lint release added a new lint the pinned version
  doesn't know about yet

## Resolution

- For vulns: update or pin the affected dependency.
- For test regressions: fix the test or pin the dependency version.
- For new lints: decide whether to fix the code or suppress the lint,
  then bump the pinned golangci-lint version in mise.toml.

This issue auto-closes when the next periodic run passes.
