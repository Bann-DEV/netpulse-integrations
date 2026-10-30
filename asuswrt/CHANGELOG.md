# Changelog

## Unreleased

- Added the external `asuswrt-pusher` entrypoint for Asuswrt and Asuswrt-Merlin.
- Reused the portable NetPulse probes without the native agent command and upgrade runtime.
- Added shared signed transport tests and on-router installation documentation.
- Added CI builds and tagged ARM64 release archives.
- Raised the core probe dependency to a build carrying the Broadcom `wl` tier, so associated clients and radio band/channel/width are collected on dhd units instead of an empty `wireless.clients`.
- Recorded the on-router validation for GT-BE98 Pro and RT-AX92U, including the gaps that remain on Broadcom (`powerDbm`, memory shared with a native agent).
