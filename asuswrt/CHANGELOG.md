# Changelog

## Unreleased

- Added the external `asuswrt-pusher` entrypoint for Asuswrt and Asuswrt-Merlin.
- Reused the portable NetPulse probes without the native agent command and upgrade runtime.
- Added shared signed transport tests and on-router installation documentation.
- Added CI builds and tagged ARM64 release archives.
- Pinned the NetPulse server's SPKI over https through `NETPULSE_SERVER_FP`, like the native agent. The self-signed certificate NetPulse issues is valid for `127.0.0.1`, so ingestion to a LAN address failed standard hostname verification before this.
