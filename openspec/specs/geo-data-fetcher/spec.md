# geo-data-fetcher Specification

## Purpose

Fetches the RIPE RU IPv4 delegation list, parses and merges it into a sorted disjoint numeric range set, persists the result to an on-disk cache, and refreshes it on a configurable schedule (with shorter retry on failure). At startup it performs a blocking first-fetch when the cache is missing or stale, then notifies the PAC generator whenever the merged ranges change.

## Requirements

### Requirement: Fetch the RU IPv4 delegation list from a configurable source

The fetcher SHALL retrieve the RIPE delegation list from the URL in `geo.source_url` (default `https://ftp.ripe.net/pub/stats/ripencc/delegated-ripencc-latest`) over HTTPS. The fetch SHALL have a timeout. A successful fetch SHALL yield the raw delegation text; an unsuccessful fetch (network error, non-2xx HTTP status, timeout) SHALL be treated as a fetch failure.

#### Scenario: Default source URL
- **WHEN** the configuration omits `geo.source_url`
- **THEN** the fetcher SHALL fetch from `https://ftp.ripe.net/pub/stats/ripencc/delegated-ripencc-latest`

#### Scenario: Custom source URL
- **WHEN** the configuration sets `geo.source_url` to a different URL
- **THEN** the fetcher SHALL fetch from that URL

#### Scenario: Fetch failure
- **WHEN** the fetch returns a non-2xx status, times out, or fails at the network layer
- **THEN** the fetcher SHALL treat the attempt as a failure
- **AND** SHALL NOT crash the process

### Requirement: Parse, sort, and merge RU IPv4 ranges

The fetcher SHALL parse the RIPE delegation format (pipe-delimited records: `registry|country|type|start|count|...`), select records where `country == RU` and `type == ipv4`, and convert each `start`+`count` pair into a numeric range `[low, high]` of uint32 values (where `low = ipToUint32(start)` and `high = low + count - 1`). The fetcher SHALL sort the ranges by `low` and merge any that are adjacent or overlapping into the minimal set of disjoint ranges. The merged result SHALL be the canonical RU range set used for PAC generation and disk caching. A parse failure (malformed input, unrecognized format) SHALL be treated as a fetch failure.

#### Scenario: Single prefix parsed correctly
- **WHEN** the delegation text contains `ripencc|RU|ipv4|1.2.3.0|256|...`
- **THEN** the parsed ranges SHALL include `[16909004, 16909259]` (1.2.3.0 through 1.2.3.255)

#### Scenario: Adjacent ranges merged
- **WHEN** the parsed ranges include both `[10, 19]` and `[20, 29]`
- **THEN** the merged result SHALL contain a single range `[10, 29]`

#### Scenario: Overlapping ranges merged
- **WHEN** the parsed ranges include `[10, 30]` and `[20, 40]`
- **THEN** the merged result SHALL contain a single range `[10, 40]`

#### Scenario: Malformed input
- **WHEN** the delegation text cannot be parsed (truncated, wrong delimiter, missing fields)
- **THEN** the fetcher SHALL treat the attempt as a failure
- **AND** SHALL NOT update the cached ranges with partial data

### Requirement: Cache merged ranges to disk

The fetcher SHALL persist the merged numeric ranges to a cache file at `~/.config/http-proxy-local/ru-cidrs.cache` (co-located with the config). The cache SHALL store the parsed+merged ranges (not raw delegation text) so that startup load requires no re-parsing. On startup, the fetcher SHALL load the cache if it exists. The cache file SHALL be written atomically (write to temp file, then rename) so that a crash mid-write cannot corrupt the cache.

#### Scenario: Cache written on successful fetch
- **WHEN** a fetch succeeds and ranges are merged
- **THEN** the merged ranges SHALL be written to `~/.config/http-proxy-local/ru-cidrs.cache`
- **AND** the write SHALL be atomic (temp-then-rename)

#### Scenario: Cache loaded on startup
- **WHEN** the process starts AND the cache file exists
- **THEN** the fetcher SHALL load the merged ranges from the cache
- **AND** SHALL NOT perform a network fetch to populate them at startup (subject to the freshness check)

### Requirement: Refresh on a configurable interval, with shorter retry on failure

The fetcher SHALL re-fetch the source on a schedule governed by two intervals:

- `geo.refresh_interval` (default `24h`): the interval used after a SUCCESSFUL refresh.
- `geo.retry_interval` (default `5m`): the shorter interval used after a FAILED refresh, repeating until a refresh succeeds.

On a successful refresh, the next attempt SHALL be scheduled at `now + refresh_interval`. On a failed refresh, the next attempt SHALL be scheduled at `now + retry_interval`; this shorter interval SHALL continue to apply on each subsequent failure until a refresh succeeds, at which point the schedule SHALL revert to `refresh_interval`. Both intervals are configurable; changes via hot-reload SHALL apply to the schedule of subsequent ticks (the in-progress wait is not shortened mid-flight). `retry_interval` SHOULD be less than `refresh_interval`; the fetcher SHALL NOT enforce this (the operator owns the configuration), but a `retry_interval >= refresh_interval` effectively disables the retry behavior.

#### Scenario: Default refresh interval
- **WHEN** the configuration omits `geo.refresh_interval`
- **THEN** the fetcher SHALL schedule the next attempt `24h` after a successful refresh

#### Scenario: Custom refresh interval
- **WHEN** the configuration sets `geo.refresh_interval` to `6h`
- **THEN** the fetcher SHALL schedule the next attempt `6h` after a successful refresh

#### Scenario: Default retry interval
- **WHEN** the configuration omits `geo.retry_interval`
- **AND** a scheduled refresh fails
- **THEN** the fetcher SHALL schedule the next attempt `5m` after the failure

#### Scenario: Custom retry interval
- **WHEN** the configuration sets `geo.retry_interval` to `2m`
- **AND** a scheduled refresh fails
- **THEN** the fetcher SHALL schedule the next attempt `2m` after the failure

#### Scenario: Retry repeats on consecutive failures
- **WHEN** a refresh fails AND the retry attempt also fails
- **THEN** the fetcher SHALL schedule another attempt `retry_interval` later
- **AND** SHALL continue retrying at `retry_interval` until a refresh succeeds

#### Scenario: Success after failures reverts to refresh_interval
- **WHEN** one or more refreshes have failed (schedule using `retry_interval`)
- **AND** the next refresh succeeds
- **THEN** the fetcher SHALL schedule the subsequent attempt at `refresh_interval`
- **AND** SHALL NOT continue using `retry_interval`

#### Scenario: Hot-reload of retry_interval applies to next tick
- **WHEN** `geo.retry_interval` changes via hot-reload while in a failure-retry loop
- **THEN** the in-progress wait SHALL complete at its original duration
- **AND** the next failure (if any) SHALL use the new `retry_interval`

### Requirement: Blocking first-fetch at startup

At process startup, the fetcher SHALL determine whether the cache is fresh: a cache is fresh if it exists AND the elapsed time since the cache's last-successful-write timestamp is less than `geo.refresh_interval`. If the cache is fresh, the fetcher SHALL load it and proceed without blocking. If the cache is missing or stale, the fetcher SHALL perform a blocking fetch BEFORE the PAC is served, so that the first PAC served reflects real RU data when obtainable. A startup fetch failure with a stale-but-present cache SHALL fall back to the stale cache (the stale data is better than none). A startup fetch failure with NO cache SHALL proceed with an empty RU range set and SHALL log the failure; the process SHALL NOT exit and SHALL continue serving (degraded mode where all non-listed hosts route to `"PROXY"`).

#### Scenario: Fresh cache skips startup fetch
- **WHEN** the process starts AND the cache exists AND was written less than `refresh_interval` ago
- **THEN** the fetcher SHALL load the cache
- **AND** SHALL NOT perform a network fetch at startup

#### Scenario: Stale cache triggers blocking startup fetch
- **WHEN** the process starts AND the cache exists AND was written more than `refresh_interval` ago
- **THEN** the fetcher SHALL perform a blocking fetch before serving the PAC
- **AND** on success SHALL update the cache and proceed
- **AND** on failure SHALL fall back to the stale cache and proceed

#### Scenario: No cache triggers blocking startup fetch
- **WHEN** the process starts AND no cache file exists
- **THEN** the fetcher SHALL perform a blocking fetch before serving the PAC
- **AND** on success SHALL write the cache and proceed
- **AND** on failure SHALL proceed with an empty RU range set (degraded mode) and log the failure
- **AND** the process SHALL NOT exit

### Requirement: Notify PAC regeneration on cache update

After the merged RU ranges are updated (whether at startup or on a scheduled refresh), the fetcher SHALL signal the PAC generator to regenerate the PAC snapshot. The signal SHALL carry the new merged ranges. The PAC generator SHALL then render and atomically swap the new PAC.

#### Scenario: Successful refresh triggers PAC regen
- **WHEN** a scheduled refresh succeeds and produces new merged ranges
- **THEN** the fetcher SHALL notify the PAC generator with the new ranges
- **AND** the PAC snapshot SHALL be regenerated

#### Scenario: Startup load triggers PAC regen
- **WHEN** the fetcher loads the cache at startup
- **THEN** the fetcher SHALL notify the PAC generator with the loaded ranges
- **AND** the PAC snapshot SHALL be generated before the first request is served
