# Changelog

All notable changes to this project will be documented in this file.

## Unreleased

### Changed
- BREAKING: `StartRenewal(ctx)` now has signature `StartRenewal()`; renewal loop lifecycle is independent of external contexts and is controlled via `Release()`/`Close()`.

### Added
- Documentation updates reflecting JetStream-based constructor.
- Internal examples now show `js := jetstream.New(conn)`.

### Planned
- Removal of deprecated wrapper in a future major release.
- Additional JetStream domain/prefix helper utilities.

## 0.1.0
Initial release.