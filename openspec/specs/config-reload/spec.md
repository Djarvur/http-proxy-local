# config-reload Specification

## Purpose

Watches the config file with fsnotify and hot-reloads it on change. Covers debounced change detection, parse/validate on reload, atomic swap of the live config (the single mutation point), and dispatching the appropriate downstream effects (upstream pickup, listen-restart notice, PAC regen, geo schedule updates) based on which fields changed.

## Requirements

### Requirement: Watch the config file with fsnotify

The system SHALL watch `~/.config/http-proxy-local/config.yaml` for changes using fsnotify. The watcher SHALL detect writes, creates, and renames that result in a new file at the watched path. The watcher SHALL debounce events so that editor save sequences (which often produce multiple write/rename events within tens of milliseconds) result in at most one reload attempt per distinct change.

#### Scenario: Config write detected
- **WHEN** the config file is modified and saved by an editor
- **THEN** the watcher SHALL emit a reload trigger after debouncing
- **AND** at most one reload SHALL occur per distinct save

#### Scenario: Atomic save (write-temp-rename) detected
- **WHEN** the config is saved via write-temp-then-rename (common editor pattern)
- **THEN** the watcher SHALL detect the rename into the watched path
- **AND** SHALL trigger exactly one reload

### Requirement: Parse and validate config on change

On each reload trigger, the system SHALL re-read and re-parse the config file into the typed Config structure. The system SHALL validate the parsed config: required fields present, `listen` parses as `host:port`, `upstream.url` is a valid `http` or `https` URL, `geo.refresh_interval` and `geo.retry_interval` are positive durations. A reload that fails parsing or validation SHALL be rejected: the system SHALL log the error, retain the previously-valid config unchanged, and continue serving on it. A rejected reload SHALL NOT swap any state.

#### Scenario: Valid config reload
- **WHEN** the config file is changed to a syntactically valid and semantically valid configuration
- **THEN** the system SHALL parse and validate it successfully
- **AND** SHALL proceed to swap it as the live config

#### Scenario: Invalid YAML rejected
- **WHEN** the config file is changed to malformed YAML
- **THEN** the system SHALL log the parse error
- **AND** SHALL retain the previous valid config
- **AND** SHALL NOT swap any state

#### Scenario: Invalid field value rejected
- **WHEN** the config file parses but `upstream.url` is not http/https
- **THEN** the system SHALL log the validation error
- **AND** SHALL retain the previous valid config
- **AND** SHALL NOT swap any state

### Requirement: Atomically swap the live config

A successful reload SHALL publish the new config via an atomic swap (e.g. `atomic.Pointer[Config].Store`). Readers (the proxy handler, the PAC generator) SHALL observe either the complete old config or the complete new config, never a partial mix. The swap SHALL be the single write-side mutation point for the live config.

#### Scenario: Concurrent reader observes consistent config
- **WHEN** a reload swaps the live config
- **AND** a reader reads the config during the swap
- **THEN** the reader SHALL observe either the complete old or the complete new config
- **AND** SHALL NOT observe a partially-applied mix

### Requirement: Trigger downstream effects on relevant changes

After a successful config swap, the system SHALL evaluate which fields changed and trigger the appropriate downstream effects:

- A change to `upstream.url`, `upstream.username`, or `upstream.password` SHALL be picked up by the proxy handler for subsequently forwarded requests (no explicit signal required beyond the atomic swap, since the handler reads the live config per-request).
- A change to `listen` SHALL log an error explaining that a restart is required (the listener is not re-bound mid-flight in v1).
- A change to `whitelist`, `blacklist`, or `listen` SHALL signal the PAC generator to regenerate the PAC snapshot.
- A change to `geo.source_url`, `geo.refresh_interval`, or `geo.retry_interval` SHALL signal the geo-data fetcher to apply the new source/interval to subsequent refreshes. A `geo.source_url` change SHALL NOT trigger an immediate fetch.

#### Scenario: Upstream change picked up per-request
- **WHEN** `upstream.url` changes in config
- **THEN** the next forwarded request SHALL use the new value (no explicit signal needed)

#### Scenario: listen change logs restart-required error
- **WHEN** the `listen` field changes in config while the server is running
- **THEN** the system SHALL log an error stating that a restart is required to apply the new listen address
- **AND** SHALL NOT rebind the listener

#### Scenario: PAC-affecting change triggers regen
- **WHEN** `whitelist`, `blacklist`, or `listen` changes in config
- **THEN** the system SHALL signal PAC regeneration

#### Scenario: geo refresh_interval change applied to schedule
- **WHEN** `geo.refresh_interval` changes in config
- **THEN** the fetcher SHALL apply the new interval to the schedule used after the next successful refresh

#### Scenario: geo retry_interval change applied to schedule
- **WHEN** `geo.retry_interval` changes in config
- **THEN** the fetcher SHALL apply the new interval to the schedule used after the next failed refresh

#### Scenario: geo source_url change does not trigger immediate fetch
- **WHEN** `geo.source_url` changes in config
- **THEN** the fetcher SHALL use the new URL on the next scheduled refresh
- **AND** SHALL NOT perform an immediate fetch
