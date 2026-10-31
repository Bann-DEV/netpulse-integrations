# FN / Realtek RTL930x switches

`fn-pusher` monitors the managed switches built on the Realtek RTL930x SDK
family (this repository's reference unit is an **FN SWITCH 8 PRO**). Their
firmware ships a sample SNMP agent and nothing else useful over SNMP: no
HOST-RESOURCES (so no CPU or memory), no LLDP-MIB (so the neighbor table is
unreadable even though LLDP is enabled and transmitted), no ENTITY-SENSOR (no
temperature), and `ifHighSpeed` answers **0 on the ports that are up** while
answering 100 Mbps on the ones with nothing plugged in. Every one of those
numbers is available, just not over SNMP. This pusher reads them where they
actually live.

## What it reports

| NetPulse field | Source |
|---|---|
| CPU %, memory % | `GET /cgi/get.cgi?cmd=sys_cpumem` |
| Port speed (`10 Gbps`) and link state | `cmd=panel_info` |
| Per-port octet counters → rates and port health | `cmd=port_cnts` |
| FDB (MAC → port) | `cmd=mac_miscStatus` |
| LLDP neighbors → evidenced topology edges | `cmd=lldp_neighbor` |
| Model, hostname, bridge MAC, uptime, **real firmware** | `cmd=sys_sysinfo` |
| Fan temperature and memory in bytes | CLI `show fan` / `show memory statistics` over SSH |

The firmware version matters: the SNMP `entPhysicalFirmwareRev` on these boxes
carries the **bootloader** version (`3.6.11.55242`), while the actual firmware
(`V1.1.6-20260913-019`) is only in `sys_sysinfo.fwVer` and the CLI.

Temperature and RAM come from the CLI, which is **optional**: if SSH is
unreachable the pusher keeps delivering everything else and those two vitals
stay `null` (never a fabricated zero). The failure is logged, because two empty
cells in the payload would otherwise look like a device that has no sensors.

The CLI is also the slow part of a poll: the login dance needs real pauses and
costs ~15 s, so its result is cached for 3 minutes and the whole exchange is
bounded by a 60 s deadline. With the default 30 s interval that means one poll
in six pays the cost, and a stuck session gives up on the deadline instead of
holding up the other integrations in the supervisor. Set `FN_CLI=0` (or
`"no_cli": true`) on a device where that trade is not worth it.

## Quirks of this firmware, all measured

- **The HTTP server gates on `User-Agent`.** A non-browser UA gets
  `{"logout":true,"reason":"notAuth"}` from every cmd, even with a valid
  session. The client sends a browser UA.
- **TLS 1.2 with static-RSA cipher suites only.** Measured with
  `openssl s_client`: the handshake lands on `AES256-GCM-SHA384`, every ECDHE
  suite fails and TLS 1.3 is refused with `alert protocol version 70`. Go
  dropped static-RSA suites from its default `ClientHello`, so they are listed
  explicitly (the ECDHE candidates stay for a firmware that grows them) and the
  minimum is TLS 1.2. The certificate is self-signed, so pin it with
  `FN_CERT_FP` / `cert_fp` (SPKI SHA-256, same format as `NETPULSE_SERVER_FP`)
  instead of reaching for `FN_INSECURE`.
- **The login body is not URL-decoded.** A `@` sent as `%40` becomes an invalid
  user, and the device still answers `save_success`, so the failure only shows
  up later as `errLoginUserInvalid`. Credentials must therefore not contain
  `&` or `=`.
- **The session cookie is issued by the third step.** `GET home_login` sets
  `login_txn`, `POST home_loginAuth` validates, and only
  `GET home_loginStatus` sets `session=`. Skipping it leaves every cmd at
  `notAuth`.
- **There are few session slots** and they are shared globally (not per
  source): logging in on every poll exhausts them and the device answers
  `errLoginSessionNum`. The client logs in once and only re-authenticates when
  a response reports `logout`.
- **The CLI needs a PTY** (without one it executes and writes nothing), and its
  `Press <Enter>` gate paints its prompt before arming the reader, so the
  first line a client sends is swallowed and username/password end up shifted
  one line. The client throws one Enter away first and then always answers the
  prompt currently on screen.
- **`show cpu`, `show memory` and `show interfaces` need sub-arguments**
  (`utilization`, `statistics`, `<1-8>`), and `show interfaces` reports
  `Auto-speed`: the negotiated rate exists **only** in the web JSON.
- Localized values arrive as i18n keys (`lang('sys','txtSysUptimeArg',[3,4,11,51])`),
  so uptime is read from the bracketed numbers instead of parsed as text.

## Configuration

| Variable | Meaning |
|---|---|
| `NETPULSE_SERVER` / `NETPULSE_SLUG` / `NETPULSE_TOKEN` | signed ingest target (shared with every integration) |
| `NETPULSE_SERVER_FP` | SPKI pin when the server is HTTPS |
| `NETPULSE_INTERVAL` | poll interval, default `30s` |
| `FN_HOST` | `https://<switch-ip>` |
| `FN_USER` / `FN_PASS` | management credentials (same account as the web UI) |
| `FN_CERT_FP` | SPKI SHA-256 of the device certificate (comma separated), the way to trust it without disabling verification |
| `FN_INSECURE=1` | accept the device's self-signed certificate |
| `FN_CLI=0` | disable the CLI probe (drops temperature and RAM) |
| `FN_SSH_ADDR` | CLI endpoint if it differs from `FN_HOST:22` |
| `FN_SSH_HOSTKEY` | `SHA256:…` of the switch SSH host key. **Required whenever `FN_PASS` is set**: the CLI login sends that password, so an unpinned key would send it to whatever answers the port. Pin it, or say you do not want the channel with `FN_CLI=0`; the pusher refuses to poll either way rather than trust silently. |

The host key fingerprint is read from the same box the web UI is served from:

```sh
ssh-keyscan -p 22 192.0.2.16 | ssh-keygen -lf -
# 2048 SHA256:9wQe3…k7Zr 192.0.2.16 (RSA)
```

A firmware update or a factory reset re-keys the device, and the symptom is one
`fn: hostkey … does not match the pinned …` line per poll. Re-read the
fingerprint and update `FN_SSH_HOSTKEY`; the web channel keeps working, so the
only loss in the meantime is the fan temperature and the memory bytes.
