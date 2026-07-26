# Contributing

This document captures the project's working agreements. They are
non-negotiable; if a change can't meet them, change the agreements first
(by editing this file + the engineering-conventions spec) rather than
shipping work that violates them.

## The three rules

1. **TDD is mandatory.** Every change starts with a failing test that
   defines the desired behavior, then the minimum implementation to make
   it pass. No production code without a corresponding test that was
   observed failing first. Refactor only on green.

2. **The strict lint gate must pass with zero findings.**
   `golangci-lint run ./...` (or `make lint` / `mise run lint`) must exit 0
   with no findings. The config (`.golangci.yml`) enables 17 linters
   including `errcheck`, `govet`, `staticcheck`, `unused`, `ineffassign`,
   `gocyclo`, `gosec`, `revive`, `gocritic`. Warnings are failures; we
   don't suppress at the linter level.

3. **Go architecture questions route through `go-ultimate`.** That skill
   (https://github.com/Djarvur/go-ultimate) is the canonical source for
   layout, conventions, and review checklists. The superseded skills
   (`go-best-practices`, `go-project-setup`, `golang-code-review`,
   `use-modern-go`) are removed from this machine and must not be
   consulted.

## Tooling setup

[mise](https://mise.jdx.dev/) pins the Go and `golangci-lint` versions so
every contributor (and CI) builds against identical toolchains:

```bash
mise install   # reads mise.toml, installs everything
```

Common tasks (defined in both `Makefile` and `mise.toml` — equivalent):

```bash
mise run build       # or: make build
mise run test        # or: make test
mise run test-race   # with the race detector (what CI runs)
mise run lint        # the gate
mise run tidy        # go mod tidy
```

## The development loop (TDD)

For any change touching logic:

1. **Write the failing test.** Pin the behavior you intend. Run it; watch
   it fail for the right reason.
2. **Write the minimum implementation.** Make the test pass. Don't add
   anything not exercised by a test.
3. **Refactor on green.** If the design improves, do it — with the test
   suite protecting you.
4. **Run `mise run lint`.** Resolve any findings at the source. Don't
   suppress with `//nolint` unless the finding is a genuine false
   positive (and document why in the comment).
5. **Commit.** Use the conventional-commit prefix (`feat:`, `fix:`,
   `test:`, `docs:`, `refactor:`).

## When a lint finding is a false positive

The strict config WILL produce false positives — `gosec` flags SSRF on
code that is, by design, a forward proxy; `gocognit` sometimes complains
about necessary complexity in state machines. Two acceptable responses:

- **Refactor.** The preferred path. If extracting a helper drops you
  under the threshold, do it.
- **Scoped `//nolint:<linter>` with a comment.** Last resort. The comment
  must explain why the finding is wrong, e.g.:
  ```go
  os.ReadFile(path) //nolint:gosec // G304: path is operator-supplied, not attacker-controlled
  ```

Never suppress at the config level (`.golangci.yml` `exclusions.rules`)
for code-specific findings. The only path-level exclusion in the config
is for the proxy package, where SSRF/log-injection findings are
intrinsic to the domain and documented as such.

## The PAC test harness

The PAC routing rules are tested two ways, both required for any change
to `internal/app/pac.go` or the routing logic:

1. **Output-shape tests** (`internal/app/pac_test.go`) — assert the
   rendered PAC contains the right injected values (listen address,
   ranges, matchers) and stays in the ES3 subset (no arrow functions,
   no `const`/`let`, no template literals).

2. **Execution-harness tests** (`internal/app/pacjs_test.go`) — spawn
   `node` to actually evaluate `FindProxyForURL` for every precedence
   scenario from the spec. This is the keystone: it caught a real CIDR
   bug during initial development.

If you change the PAC template, run the harness. node must be installed
locally; CI installs it via `actions/setup-node`.

## Real-proxy smoke tests

Tests against a real upstream proxy (not in-process stubs) live in
`internal/in/http/realchain_test.go` and are gated behind the
`realproxy` build tag. They're excluded from `go test ./...` and CI by
default.

When to run them:

- After changing `dialUpstream`, `dialProxy`, `tlsDial`, or anything in
  the CONNECT tunneling path.
- After changing how credentials are formatted or sent.
- Before any release that touches the proxy-chain code.

How to run:

```bash
# Credentials come from the env, never hardcoded. The URL can include
# user:pass@host or you can set them via the URL itself.
ZCODE_HTTP_PROXY=https://user:pass@host:port \
  go test -tags realproxy -run TestRealProxy ./internal/in/http/
```

If your env doesn't have `ZCODE_HTTP_PROXY`, `HTTP_PROXY`, or
`HTTPS_PROXY` set, the tests skip cleanly.

## The PAC `_js` build-tag gotcha

Go treats the `_js` file suffix as a build constraint (GOOS=js). A file
named `pac_js_test.go` is silently dropped from the test binary. Use
`pacjs_test.go` or any name without `_js`/`_linux`/etc. If a test
silently disappears from `go test -list`, check the filename first.

## OpenSpec workflow

Substantive changes go through OpenSpec:

```bash
/opsx:new-change <name>     # create a change (proposal/specs/design/tasks)
/opsx:apply <name>          # implement tasks
/opsx:archive <name>        # archive once complete
```

The current change history lives in `openspec/changes/` (active) and
`openspec/archive/` (completed). The `engineering-conventions` spec in
the active change is the source of truth for the three rules above — if
this doc and that spec disagree, **the spec wins** (and this doc should
be updated).

## What not to do

- Don't write implementation before the test.
- Don't suppress a lint finding without a comment explaining why.
- Don't add `.go` files at the module root — all Go source lives under
  `cmd/` (entrypoint + wiring) or `internal/` (everything else).
- Don't add a `pkg/` directory. `go-ultimate`'s Hexagonal layout puts
  library code at the module root or under `internal/`.
- Don't consult superseded Go skills. `go-ultimate` is the only one.
- Don't commit credentials or anything read from a user-specific
  environment variable.
