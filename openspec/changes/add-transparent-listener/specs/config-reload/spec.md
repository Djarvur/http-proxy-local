# Spec Delta

## ADDED Requirements

### Requirement: Validate the transparent listen address

The system SHALL validate the `transparent.listen` field when the
`transparent` block is present: a non-empty value SHALL parse as `host:port`;
an empty value or an absent block SHALL be valid and mean the transparent
listener is disabled (no bind). A violation SHALL reject the reload: the
system SHALL log the error naming the `transparent.listen` field, retain the
previously-valid config unchanged, and continue serving on it.

#### Scenario: Invalid transparent.listen rejected

- **WHEN** the config file parses but `transparent.listen` is non-empty and
  does not parse as `host:port`
- **THEN** the system SHALL log the validation error naming the
  `transparent.listen` field
- **AND** SHALL retain the previous valid config
- **AND** SHALL NOT swap any state

#### Scenario: Empty transparent.listen accepted as disabled

- **WHEN** the config file parses with `transparent.listen` empty (whether the
  `transparent` block is absent or the field is omitted)
- **THEN** the system SHALL validate it successfully
- **AND** SHALL treat the transparent listener as disabled (no bind)

### Requirement: Detect transparent listener changes on reload

After a successful config swap, a change to `transparent.listen` SHALL log an
error explaining that a restart is required (the transparent listener is not
re-bound, started, or stopped mid-flight). This applies to all transitions:
changing the bound address, adding the `transparent` block when it was
previously absent, and removing it when it was previously present.

#### Scenario: transparent.listen change logs restart-required error

- **WHEN** the `transparent.listen` field changes in config while the server
  is running (including the address changing, the block being added, or the
  block being removed)
- **THEN** the system SHALL log an error stating that a restart is required
  to apply the transparent listener change
- **AND** SHALL NOT rebind, start, or stop the transparent listener mid-flight
