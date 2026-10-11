# ADR 0007: Installer and distribution

**Status:** accepted

## Decision
- Each release publishes a `manifest.json` listing every artifact (OS, architecture, URL, SHA-256, size) plus `SHA256SUMS`. Signing with minisign is supported when a key is configured; until then the installer verifies checksums only and says so.
- `scripts/install.sh` detects OS and CPU, downloads the manifest from a base URL given by `LUX_RELEASE_BASE` (no default domain is assumed until one exists), verifies the archive's SHA-256 before extracting, and installs to `~/.local/lib/lux` with links in `~/.local/bin`. It never uses `sudo`.
- `install.sh --uninstall` removes exactly what it installed. Upgrades install side by side and switch a `current` link, so a failed upgrade leaves the old version working.
- Desktop packages are built with `tauri build` (`.deb`, `.AppImage` on Linux; `.dmg` on macOS; `.msi` on Windows) in CI.
- macOS and Linux are first. Windows: `luxd` and `lux` build for Windows, the desktop app builds with WebView2; the `local` executor's process-group handling is POSIX-only and needs a Windows job-object implementation before it is supported.

## Consequences
- Nothing executes before its checksum matches.
- Until the release domain exists, `LUX_RELEASE_BASE` must be set (for example to a GitHub Releases URL).
