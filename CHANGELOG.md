# Changelog

All notable changes to this project will be documented in this file.

## [1.1.0] - 2026-09-02

### Added

- Added live FFmpeg output streaming through `pipe:1` with completion status and byte-count HTTP trailers.
- Added request validation for commands, input sources, safe filenames, output combinations, media types, and required S3 configuration.
- Added cancellation of input transfers and FFmpeg processes when a request ends.

### Changed

- Unified buffered and streaming FFmpeg execution to remove duplicate process flows.
- Hardened temporary directory creation, output discovery, S3 URL parsing, and filesystem error handling.
- Reworked request and lifecycle logging so startup, FFmpeg steps, failures, and cleanup are reported consistently.
- Changed the runtime image to execute as an unprivileged user with a private writable temporary directory.
- Expanded the README with live streaming, trailer handling, validation rules, and deployment guidance.

### Removed

- Removed obsolete and explanatory comments from the Go source.
- Removed the UUID dependency by using secure operating-system temporary directories.

## [1.0.2] - 2025-12-23

### Added

- Logging support to improve observability and debugging.
- A new temporary input type, allowing indeterminate files to be processed without producing output artifacts.

## [1.0.1] - 2025-12-20

### Fixed

- Resolved an issue where the first streamed output could incorrectly return the input file.

### Added

- Added S3 configuration documentation to the README.
- Expanded README with an explanation of parallel processing behavior.

## [1.0.0] - 2025-12-20

### Added

- Initial commit with FFmpeg HTTP API implementation.
