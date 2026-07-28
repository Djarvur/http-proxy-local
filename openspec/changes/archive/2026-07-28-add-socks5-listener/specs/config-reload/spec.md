## MODIFIED Requirements

### Requirement: Parse and validate config on change

On each reload trigger, the system SHALL re-read and re-parse the config file into the typed Config structure. The system SHALL validate the parsed config: required fields present, `listen` parses as `host:port`, `upstream.url` is a valid `http` or `https` URL (or empty for direct mode), `geo.refresh_interval` and `geo.retry_interval` are positive durations, `pac.cache_seconds` (when set) is non-negative, and `socks5.listen` (when non-empty) parses as `host:port`. A reload that fails parsing or validation SHALL be rejected: the system SHALL log the error, retain the previously-valid config unchanged, and continue serving on it. A rejected reload SHALL NOT swap any state.

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

#### Scenario: Invalid socks5.listen rejected
- **WHEN** the config file parses but `socks5.listen` is non-empty and does not parse as `host:port`
- **THEN** the system SHALL log the validation error naming the `socks5.listen` field
- **AND** SHALL retain the previous valid config
- **AND** SHALL NOT swap any state

#### Scenario: Empty socks5.listen accepted as disabled
- **WHEN** the config file parses with `socks5.listen` empty (whether the `socks5` block is absent or the field is omitted)
- **THEN** the system SHALL validate it successfully
- **AND** SHALL treat the SOCKS5 listener as disabled (no bind)

### Requirement: Trigger downstream effects on relevant changes

After a successful config swap, the system SHALL evaluate which fields changed and trigger the appropriate downstream effects:

- A change to `upstream.url`, `upstream.username`, or `upstream.password` SHALL be picked up by the proxy handler for subsequently forwarded requests (no explicit signal required beyond the atomic swap, since the handler reads the live config per-request).
- A change to `pac.cache_seconds` SHALL be picked up by the `/proxy.pac` handler on the next request (no explicit signal required — the handler reads the live config per-request to derive the `Cache-Control` header).
- A change to `listen` SHALL log an error explaining that a restart is required (the listener is not re-bound mid-flight in v1).
- A change to `whitelist`, `blacklist`, or `listen` SHALL signal the PAC generator to regenerate the PAC snapshot.
- A change to `geo.source_url`, `geo.refresh_interval`, or `geo.retry_interval` SHALL signal the geo-data fetcher to apply the new source/interval to subsequent refreshes. A `geo.source_url` change SHALL NOT trigger an immediate fetch.
- A change to `socks5.listen` SHALL log an error explaining that a restart is required (the SOCKS5 listener is not re-bound, started, or stopped mid-flight). This applies to all transitions: changing the bound address, adding the `socks5` block when it was previously absent, and removing it when it was previously present.

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

#### Scenario: socks5.listen change logs restart-required error
- **WHEN** the `socks5.listen` field changes in config while the server is running (including the address changing, the `socks5` block being added, or the `socks5` block being removed)
- **THEN** the system SHALL log an error stating that a restart is required to apply the SOCKS5 listener change
- **AND** SHALL NOT rebind, start, or stop the SOCKS5 listener mid-flight
