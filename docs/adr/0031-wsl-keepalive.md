# ADR-0031: Keep the WSL node alive with a boot task and instanceIdleTimeout=-1

- Status: accepted
- Date: 2026-11-13

## Context
WSL starts a distro only when something runs in it. It shuts an idle distro down after `[general] instanceIdleTimeout` (default 15 s), and then the VM after `[wsl2] vmIdleTimeout`. A node must be reachable over the LAN after a reboot, before anyone logs in, and must stay up while idle.

## Decision
Use both mechanisms:
1. A scheduled task named "docker-offload-lan keep WSL alive", triggered at startup and at logon. It runs `wsl.exe -d Ubuntu-24.04 -u root --exec /bin/sleep infinity` as the installing user with logon type S4U (no stored password), with no execution time limit and restart on failure. Starting the distro boots systemd, which starts docker, ssh and dol-node. The sleeping process keeps the distro running.
2. `[general] instanceIdleTimeout=-1` in `.wslconfig` as a second guard: if the task's process dies, the distro still does not stop for idleness.

`vmIdleTimeout` is left at its default, because the VM cannot idle out while an instance is running.

## Consequences
- The node comes back after reboots and power cuts without anyone logging in. This depends on S4U working with WSL on the installed Windows build; setup-windows.md explains how to switch the task to "run only when user is logged on" if it does not.
- `-NoKeepAliveTask` skips the task for users who prefer to start WSL by hand.

## Alternatives
- Only `instanceIdleTimeout=-1`: rejected, because nothing would start the distro at boot.
- Only the task: rejected as the sole mechanism, because a `wsl --shutdown` or a crash of the sleep process would let the idle timer stop the distro.
- A Windows service wrapper (nssm etc.): rejected as an extra dependency.
