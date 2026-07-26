# engineering-conventions Specification

## Purpose

Project-wide engineering rules that govern all production code in this repository. Covers the mandatory Test-Driven Development workflow (red/green/refactor), the strict golangci-lint gate with zero findings, and the designation of `go-ultimate` as the canonical Go skill that supersedes the older Go skills.

## Requirements

### Requirement: Test-Driven Development is mandatory

All production code in this project SHALL be developed using Test-Driven Development (TDD), strictly in the red/green/refactor cycle: a failing test that defines the desired behavior is written FIRST, then the minimum implementation required to make that test pass, then refactoring only while tests remain green. No production code SHALL be written without a corresponding test existing first that exercises the new behavior. Refactoring SHALL NOT introduce new behavior; it SHALL be performed only with passing tests.

"Production code" means any non-test, non-vendored, non-generated source file. Build scaffolding (`go.mod`, `.golangci.yml`, `Makefile`) and the PAC template (which is data, not logic) are exempt only when no testable behavior exists; where the template's behavior IS testable (e.g. precedence ordering, RU lookup), it SHALL be covered by tests.

#### Scenario: New function is added
- **WHEN** a contributor adds a new exported or unexported function to a production package
- **THEN** at least one test SHALL already exist (committed in the same change) that calls that function and asserts on its observable behavior
- **AND** that test SHALL have been observed failing before the implementation was written

#### Scenario: Behavior changes in an existing function
- **WHEN** the observable behavior of an existing function is modified (new return value, new error condition, changed side effect)
- **THEN** a test SHALL be added or modified first to express the new expected behavior
- **AND** the test SHALL fail against the pre-change implementation

#### Scenario: Bug fix
- **WHEN** a bug is fixed
- **THEN** a regression test SHALL be written first that reproduces the bug
- **THEN** the test SHALL fail before the fix is applied and pass after

#### Scenario: Refactor without new behavior
- **WHEN** code is refactored without changing observable behavior
- **THEN** no new tests are required
- **AND** the existing test suite SHALL remain green throughout

### Requirement: Strict golangci-lint gate with zero findings

The repository SHALL contain a strict `.golangci.yml` at the repo root. `golangci-lint run` SHALL exit with code 0 and produce zero findings on the entire codebase. The lint configuration SHALL enable at minimum: `errcheck`, `govet`, `staticcheck`, `unused`, `ineffassign`, `gocyclo`, `gosec`, `revive`, and `gocritic`. Findings of any severity (warnings included) SHALL be treated as failures. The lint gate SHALL be the same locally and in any CI configuration.

#### Scenario: Fresh clone passes lint
- **WHEN** the repository is freshly cloned
- **AND** `golangci-lint run` is executed from the repo root
- **THEN** the command SHALL exit with code 0
- **AND** SHALL produce zero findings

#### Scenario: Lint config is present and strict
- **WHEN** the `.golangci.yml` file is inspected
- **THEN** it SHALL enable the required linters (`errcheck`, `govet`, `staticcheck`, `unused`, `ineffassign`, `gocyclo`, `gosec`, `revive`, `gocritic`)
- **AND** no linter in the enabled set SHALL be configured to skip errors or downgrade failures to non-blocking

#### Scenario: New code introducing a lint finding is rejected
- **WHEN** a change introduces code that would produce any golangci-lint finding
- **THEN** the change SHALL be considered incomplete
- **AND** SHALL NOT be archived until the finding is resolved at the source (not suppressed)

### Requirement: go-ultimate is the canonical Go skill

All Go architecture, convention, layout, and review guidance for this project SHALL be sourced from the `go-ultimate` skill (https://github.com/Djarvur/go-ultimate). The skills `go-best-practices`, `go-project-setup`, `golang-code-review`, and `use-modern-go` are superseded by `go-ultimate` and SHALL NOT be consulted or invoked. When `go-ultimate` and another source conflict, `go-ultimate` SHALL take precedence unless an explicit project-level decision (recorded in this change's design.md or a spec) overrides it.

#### Scenario: Go architecture question arises
- **WHEN** a question about Go project layout, conventions, or patterns arises during design or implementation
- **THEN** the `go-ultimate` skill SHALL be consulted
- **AND** the superseded skills (go-best-practices, go-project-setup, golang-code-review, use-modern-go) SHALL NOT be invoked

#### Scenario: Conflict between go-ultimate and another source
- **WHEN** guidance from `go-ultimate` conflicts with another source (older skill, memory, or generic best-practice memory)
- **THEN** `go-ultimate` SHALL take precedence
- **UNLESS** an explicit project-level decision documented in `design.md` or a spec overrides it for this project
