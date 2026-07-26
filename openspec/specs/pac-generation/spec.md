# pac-generation Specification

## Purpose

Renders the Proxy Auto-Configuration (PAC) document from the live configuration and cached RU IPv4 ranges. Covers PAC bytes generation, the strict routing precedence (blacklist → whitelist → local-network → RU geo → default), local/private network handling, the binary-search RU lookup, regeneration triggers, and the v1 IPv4-only constraint.

## Requirements

### Requirement: Generate PAC bytes from config and cached RU ranges

The system SHALL render the PAC document by combining: (a) the live configuration's `listen` address, whitelist, and blacklist; (b) the cached merged RU IPv4 numeric ranges. The PAC SHALL be valid PAC-engine JavaScript (ES3/ES5-subset: `var`, `function`, no arrow functions, no template literals, no `const`/`let`). The PAC bytes SHALL be held in an atomically-swappable snapshot; the proxy server's `/proxy.pac` handler SHALL always return the current snapshot.

#### Scenario: PAC reflects current config
- **WHEN** the live config has `listen: 127.0.0.1:8080`, whitelist `[*.a.com]`, blacklist `[*.b.com]`
- **THEN** the rendered PAC SHALL contain the `PROXY 127.0.0.1:8080` directive
- **AND** SHALL contain JavaScript that matches `*.a.com` as whitelist and `*.b.com` as blacklist

#### Scenario: PAC reflects current RU ranges
- **WHEN** the cached RU ranges contain the numeric range for `1.2.3.0/24`
- **THEN** the rendered PAC SHALL contain a `RU_RANGES` entry that causes `inRu(ipToInt("1.2.3.5"))` to return true

#### Scenario: PAC is valid PAC-engine JavaScript
- **WHEN** the rendered PAC is parsed by a standards-compliant PAC engine
- **THEN** it SHALL parse without syntax errors
- **AND** SHALL NOT contain arrow functions, template literals, `const`, or `let`

### Requirement: Apply the routing precedence strictly

The PAC's `FindProxyForURL(url, host)` SHALL evaluate routing in this exact precedence order, returning the result of the first matching rule:

1. **blacklist** match → `"DIRECT"` (absolute; overrides whitelist, local-network, and geo)
2. **whitelist** match → `"PROXY <listen>"` (overrides local-network and geo)
3. **local/private network** → `"DIRECT"`
4. **RU IPv4** (binary search over sorted merged ranges) → `"DIRECT"`
5. default → `"PROXY <listen>"`

A host SHALL be considered to match the blacklist or whitelist if it matches by suffix (`shExpMatch` semantics: `*.example.com` matches `example.com` and any subdomain; bare `example.com` matches `example.com` and subdomains) OR by CIDR (`isInNet` / numeric range check on the resolved IP).

#### Scenario: Blacklist overrides whitelist
- **WHEN** host `x.example.com` is in both the whitelist and the blacklist
- **THEN** `FindProxyForURL` SHALL return `"DIRECT"`

#### Scenario: Whitelist overrides local-network
- **WHEN** host `10.0.0.5` matches a whitelist CIDR entry `10.0.0.0/8`
- **THEN** `FindProxyForURL` SHALL return `"PROXY <listen>"`
- **EVEN THOUGH** `10.0.0.5` is in a local-network range

#### Scenario: Whitelist overrides RU geo
- **WHEN** host `example.ru` resolves to a RU IP AND `example.ru` is in the whitelist
- **THEN** `FindProxyForURL` SHALL return `"PROXY <listen>"`

#### Scenario: Local-network not in any list
- **WHEN** host `192.168.1.1` is not in whitelist or blacklist
- **THEN** `FindProxyForURL` SHALL return `"DIRECT"`

#### Scenario: RU host not in any list
- **WHEN** host `example.ru` resolves to a RU IPv4 address and is not in any list
- **THEN** `FindProxyForURL` SHALL return `"DIRECT"`

#### Scenario: Non-RU host not in any list
- **WHEN** host `example.com` resolves to a non-RU IPv4 address and is not in any list
- **THEN** `FindProxyForURL` SHALL return `"PROXY <listen>"`

#### Scenario: Suffix match
- **WHEN** the whitelist contains `*.corp.example.com`
- **AND** the host is `api.corp.example.com`
- **THEN** the whitelist SHALL match (returning `"PROXY <listen>"`)

#### Scenario: CIDR match
- **WHEN** the blacklist contains `10.0.0.0/8`
- **AND** the host is `10.5.5.5`
- **THEN** the blacklist SHALL match (returning `"DIRECT"`)

### Requirement: Treat local/private networks as DIRECT

The PAC SHALL treat the following as local/private and route them `DIRECT` unless an explicit whitelist entry overrides them: plain hostnames (no dots, per `isPlainHostName`), loopback (`127.0.0.0/8`), RFC1918 private ranges (`10.0.0.0/8`, `172.16.0.0/12`, `192.168.0.0/16`), and link-local (`169.254.0.0/16`). The blacklist is evaluated before this rule and therefore also yields `DIRECT` for local addresses, but only the whitelist can force a local address to `PROXY`.

#### Scenario: Plain hostname
- **WHEN** the host is `localhost` (no dots)
- **THEN** `FindProxyForURL` SHALL return `"DIRECT"`

#### Scenario: Loopback
- **WHEN** the host is `127.0.0.1`
- **THEN** `FindProxyForURL` SHALL return `"DIRECT"`

#### Scenario: RFC1918 address
- **WHEN** the host is `10.1.2.3` and not whitelisted
- **THEN** `FindProxyForURL` SHALL return `"DIRECT"`

### Requirement: Binary-search RU lookup over merged ranges

The PAC SHALL encode the RU IPv4 space as a sorted array of disjoint `[low, high]` uint32 pairs, where ranges have been merged across adjacency and overlap. The lookup function `inRu(n)` SHALL perform a binary search over this array and return true iff `n` falls in any range. When the RU range cache is empty (fetch failure with no prior cache), `RU_RANGES` SHALL be an empty array and `inRu` SHALL return false for all inputs (every non-listed host falls through to `"PROXY <listen>"`).

#### Scenario: Empty RU ranges degrade to all-proxy
- **WHEN** the RU cache is empty
- **AND** host `example.ru` resolves to a RU IP
- **AND** the host is in no list
- **THEN** `FindProxyForURL` SHALL return `"PROXY <listen>"` (graceful degradation)

#### Scenario: IP inside a merged range
- **WHEN** the RU ranges include `[167772160, 184549375]`
- **AND** the host resolves to `10.0.0.1` (= 167772161)
- **THEN** `inRu(167772161)` SHALL return true

### Requirement: Regenerate PAC on config or cache change

The system SHALL regenerate the PAC snapshot whenever (a) the live configuration changes in a field that affects PAC output (`listen`, `whitelist`, `blacklist`) OR (b) the cached RU ranges change (post-fetch). Regeneration SHALL swap the snapshot atomically; the `/proxy.pac` handler SHALL see either the complete old PAC or the complete new PAC, never a partial mix. Fields that do not affect PAC output (e.g. `upstream.*`, `geo.refresh_interval`) SHALL NOT trigger regeneration.

#### Scenario: PAC regenerates on whitelist change
- **WHEN** the whitelist is edited in config
- **THEN** a new PAC snapshot SHALL be generated and atomically swapped
- **AND** the next `GET /proxy.pac` SHALL return the new bytes

#### Scenario: PAC regenerates on RU cache refresh
- **WHEN** the geo-data fetcher writes new merged RU ranges
- **THEN** a new PAC snapshot SHALL be generated and atomically swapped

#### Scenario: PAC does NOT regenerate on upstream-only change
- **WHEN** only `upstream.url` changes in config
- **THEN** the PAC snapshot SHALL remain unchanged (the PAC carries no upstream detail)

### Requirement: IPv4-only geo in v1

For v1, the PAC's RU lookup SHALL operate on IPv4 only. `dnsResolve(host)` returning an IPv6 address (containing `:`) SHALL be treated as non-matching (the host falls through to local/whitelist/blacklist/default logic; if none of those match, the result is `"PROXY <listen>"`). IPv6 RU ranges SHALL NOT be present in v1 PACs. This constraint SHALL be removable in a future change without changing the precedence rules or PAC shape.

#### Scenario: IPv6 resolution falls through
- **WHEN** `dnsResolve(host)` returns `::1`
- **AND** the host is in no list and is not a local-network literal
- **THEN** `FindProxyForURL` SHALL return `"PROXY <listen>"`
