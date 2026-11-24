# Changelog

All notable changes to **UxPlay Desktop** will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Fixed
- **Mirror Receiver Lifecycle State Consistency**: Reworked the AirPlay mirror receiver start/stop/thread-exit paths so the "running" flag, the joined receiver thread and the listening socket always converge together. A receiver thread that exits because the client closed the stream, the connection was reset, an unsupported video codec was offered, or an explicit stop was requested now closes both the per-connection socket and the listening socket, clears the running flag and notifies the upper layer exactly once through a single teardown path.
- **Repeated Stop Safety**: `stop` is fully idempotent — repeated calls return immediately without blocking, re-notifying the upper layer or touching released sockets, and can no longer deadlock against a thread that is itself shutting down.
- **Restart Guard**: Starting the receiver while it is running, or while a previously exited receiver thread has not yet been reaped, is now rejected (and disconnects the request) instead of orphaning a thread and leaking a socket/port.
- **Pause/Resume vs. Disconnect Races**: Pause and resume callbacks are serialized with thread teardown. A pause followed by a disconnect now always ends in "stopped" (never stuck on "paused"), and a resume packet processed during the disconnect window can no longer flip the state back to "mirroring".
- **Unsupported Codec Disconnect Reason**: When the receiver drops a connection because the client offered an unsupported/rejected video codec, it goes through the same teardown path as a normal disconnect and reports a distinct reason code (`conn_reset` reason 3) so the upper layer can tell the two cases apart.
- **Half-Open Connection Robustness**: A client that connects and vanishes before sending a full packet header no longer tears down the whole mirror service; the receiver drops the half-open connection and returns to accepting.

### Added
- **Native Mirror Receiver Lifecycle Tests**: Added 13 automated C tests under `tests/mirror_receiver_tests` that drive the real receiver thread over loopback TCP (no iPad, Bonjour or GStreamer required), covering start/stop, idempotent stop, restart refusal, client close, connection reset, unsupported/rejected codec, pause-then-disconnect, late-resume-after-disconnect, port release and repeated lifecycle cycles. Wired into the CI build.

## [1.2.5] - 2026-11-24

### Added
- **Silent Background Auto-Updates**: UxPlay Desktop now checks for and downloads new releases automatically in the background without user intervention.
- **Active Session Safety Guard**: Auto-update restarts are deferred whenever an AirPlay mirroring or streaming session is in progress, preventing any interruption to presentations, classes, or live video. The update is applied automatically and silently as soon as the session ends.
- **Auto-Update Toggle Setting**: Added an option in Settings (`Settings.AutoUpdate`, enabled by default) with localized descriptions in 16 languages.
- **Target Display Monitor Refresh**: Added an on-demand refresh button in Settings to re-scan connected displays dynamically.

### Fixed
- **Target Display Monitor Persistence**: Fixed an issue where the selected display monitor field appeared empty upon reopening the application due to reference equality mismatch in WPF ComboBox data binding.
- **Automatic Streaming Window Placement**: UxPlay now reliably detects the creation of the GStreamer video rendering window and immediately positions/centers it on the user's selected target monitor.
- **UI Glyph Bug in Network Doctor Button**: Replaced the Unicode 12 font-dependent stethoscope emoji (`🩺`) with a crisp, resolution-independent vector path that seamlessly adapts to the user's chosen accent color and renders reliably across all Windows versions.

## [1.2.4] - 2026-11-21

### Added
- **Network Adapter Selection**: Added dedicated network adapter selector in Settings (`-i <ipv4>`). AirPlay can now be advertised on a specific network interface (e.g. Ethernet LAN, Wi-Fi, or Tailscale / VPN tunnels) instead of relying solely on default routing.
- **Dynamic Adapter Resolution & Refresh**: Automatic resolution of adapter changes, gateway priority ranking, and a manual Refresh button for dynamic network setups.
- **Multilingual Support for Network Settings**: Translated all network adapter controls and helper descriptions across all 16 supported languages.
- **Winget Distribution**: Official Windows Package Manager support (`winget install jorjanion.UxPlayDesktop`).

### Changed
- **Comparison Table Reordered**: Prominently positioned UxPlay Desktop in the comparison matrix for clearer reading.
- **CI/CD Optimization**: Streamlined build and test workflows to trigger on releases.

## [1.2.3] - 2026-11-19

### Fixed
- **Updater Self-Termination Prevention**: Removed `/T` flag from `taskkill` in the installer to prevent prematurely terminating the updater when spawned from the running application.
- **Streaming Preset Latency Display**: Added expected latency annotations in milliseconds for each streaming preset option.

## [1.2.2] - 2026-11-15

### Fixed
- **Streaming Preset Translation Reactivity**: Synchronized preset descriptions and latency labels dynamically when changing language.
- **Toolbar Localization**: Ensured all toolbar action tooltips and button labels update instantly upon language change.

## [1.2.1] - 2026-11-10

### Fixed
- **AirPlay Disconnect Detection**: Immediately detect disconnects from device mirror stop and TCP socket close events.

## [1.2.0] - 2026-11-05

### Added
- **Windows on ARM (ARM64) Support**: Full native support for ARM64 Windows devices with dedicated CI build matrix and native installer.
