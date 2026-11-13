#!/usr/bin/env bash
# docker-offload-lan node installer (Ubuntu/Debian; WSL2 or plain Linux).
#
# Idempotent: every step prints "[changed]", "[unchanged]", "[skipped]" or
# "[would change]" (with --dry-run). A second run changes nothing.
#
#   sudo ./install.sh --pubkey ~/id_ed25519.pub [--dol-binary ./dol] [--node-name tower]
#
# Run `./install.sh --help` for all flags. See docs/setup-windows.md and
# docs/security.md. Independent community project; not affiliated with Docker, Inc.
set -euo pipefail

# --- pinned versions (keep in sync with internal/doctor/pins.go; a Go test checks) ---
KIND_VERSION="v0.33.0"
KUBECTL_VERSION="v1.37.2"
# sha256 of the official release binaries (from kind-linux-<arch>.sha256sum and
# dl.k8s.io/.../kubectl.sha256, fetched 2026-11-13). Verified after download.
pinned_sha256() { # pinned_sha256 <kind|kubectl> <amd64|arm64>
  case "$1/$2" in
    kind/amd64)    echo "aee6151561422756b764a4ae28e7f44cda5af5a9eead3cc9985112b1de8d8e0d" ;;
    kind/arm64)    echo "20022bee6cfcd5086cb7234d218e3454e6090022f2a8f55d1fa7fcf42c3867a2" ;;
    kubectl/amd64) echo "c04487523e7aa7d1493f7808174c899db93d7a9cf57531420cd76b9bd415f8f4" ;;
    kubectl/arm64) echo "fcc16f63186659d9722014b7e02d5fdd2d4e669cb8c0ab66365d7d3b8ba03509" ;;
    *) return 1 ;;
  esac
}

DOL_REPO="fl0wo/docker-offload-lan"
DOL_USER="dol"
DOL_GROUP="dol"
SSH_PORT="2222"
SYNC_ROOT="/srv/dol"
CONF_DIR="/etc/docker-offload-lan"
NODE_YAML="${CONF_DIR}/node.yaml"
STATE_DIR="/var/lib/docker-offload-lan"
RUN_DIR="/run/docker-offload-lan"
SSHD_DROPIN="/etc/ssh/sshd_config.d/10-docker-offload-lan.conf"
DAEMON_JSON="/etc/docker/daemon.json"
UNIT_PATH="/etc/systemd/system/dol-node.service"
BIN_DIR="/usr/local/bin"

# --- flags ---
PUBKEYS=()
DOL_BINARY=""
DOL_VERSION=""
NODE_NAME=""
SSHD_MODE="auto"     # auto | only-2222 | alongside
CONTAINER_MODE=0
DRY_RUN=0
SKIP_DOCTOR=0
NO_DOL=0
BUILDKIT_MAX_USED="40GB"
SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"

usage() {
  cat <<'EOF'
Usage: sudo install.sh [flags]

  --pubkey <file|key>    SSH public key allowed to log in as `dol` (repeatable).
  --dol-binary <path>    Install this dol binary (else: GitHub release, see --dol-version).
  --dol-version <vX.Y.Z> Download this dol release from GitHub (default: latest, only if
                         dol is not installed yet).
  --node-name <name>     Node name written to node.yaml (default: hostname).
  --sshd-mode <mode>     only-2222 | alongside (keep port 22 too) | auto (default:
                         only-2222 on WSL and in containers, alongside on plain Linux).
  --buildkit-max <size>  BuildKit cache cap (builder.gc.defaultMaxUsedSpace), default 40GB.
  --container-mode       No systemd: for the integration-test image. Services are not
                         enabled; the container entrypoint starts dockerd, sshd and
                         `dol node serve`.
  --no-dol               Do not install the dol binary (you install it another way).
  --skip-doctor          Do not run `dol doctor --node` at the end.
  --dry-run              Print what would change; change nothing.
  -h, --help             This help.
EOF
}

die() { echo "install.sh: error: $*" >&2; exit 1; }

while [[ $# -gt 0 ]]; do
  case "$1" in
    --pubkey) [[ $# -ge 2 ]] || die "--pubkey needs a value"; PUBKEYS+=("$2"); shift 2 ;;
    --dol-binary) [[ $# -ge 2 ]] || die "--dol-binary needs a path"; DOL_BINARY="$2"; shift 2 ;;
    --dol-version) [[ $# -ge 2 ]] || die "--dol-version needs a value"; DOL_VERSION="$2"; shift 2 ;;
    --node-name) [[ $# -ge 2 ]] || die "--node-name needs a value"; NODE_NAME="$2"; shift 2 ;;
    --sshd-mode) [[ $# -ge 2 ]] || die "--sshd-mode needs a value"; SSHD_MODE="$2"; shift 2 ;;
    --buildkit-max) [[ $# -ge 2 ]] || die "--buildkit-max needs a value"; BUILDKIT_MAX_USED="$2"; shift 2 ;;
    --container-mode) CONTAINER_MODE=1; shift ;;
    --skip-doctor) SKIP_DOCTOR=1; shift ;;
    --no-dol) NO_DOL=1; shift ;;
    --dry-run) DRY_RUN=1; shift ;;
    -h|--help) usage; exit 0 ;;
    *) usage >&2; die "unknown flag: $1" ;;
  esac
done

case "$SSHD_MODE" in auto|only-2222|alongside) ;; *) die "--sshd-mode must be auto, only-2222 or alongside" ;; esac
[[ "$BUILDKIT_MAX_USED" =~ ^[0-9]+(MB|GB|TB)$ ]] || die "--buildkit-max must look like 40GB"

# Re-exec as root (dry-run may run unprivileged, it only reads).
if [[ $EUID -ne 0 && $DRY_RUN -eq 0 ]]; then
  command -v sudo >/dev/null || die "run as root"
  echo "install.sh: re-running with sudo" >&2
  exec sudo -- bash "${BASH_SOURCE[0]}" "$@"
fi

# Resolve user-supplied paths before anything changes directory.
if [[ -n "$DOL_BINARY" ]]; then
  [[ -f "$DOL_BINARY" ]] || die "--dol-binary $DOL_BINARY: no such file"
  DOL_BINARY="$(cd -- "$(dirname -- "$DOL_BINARY")" && pwd)/$(basename -- "$DOL_BINARY")"
fi

export DEBIAN_FRONTEND=noninteractive

# --- reporting ---
CHANGES=0
report() { # report <state> <message>
  local state="$1"; shift
  [[ "$state" == changed || "$state" == "would change" ]] && CHANGES=$((CHANGES + 1))
  printf '[%s] %s\n' "$state" "$*"
}
changed_or_would() { if [[ $DRY_RUN -eq 1 ]]; then echo "would change"; else echo changed; fi; }
run() { # run a mutating command (skipped with --dry-run)
  if [[ $DRY_RUN -eq 1 ]]; then echo "  would run: $*"; return 0; fi
  "$@"
}
tmpdir="$(mktemp -d)"
trap 'rm -rf "$tmpdir"' EXIT

# put_file <path> <mode> <owner:group> <label>   (content on stdin)
# Writes atomically only when content, mode or owner differ. Returns 0 if the
# file changed (or would change), 1 if unchanged.
put_file() {
  local path="$1" mode="$2" owner="$3" label="$4" new="$tmpdir/put.$RANDOM"
  cat >"$new"
  if [[ -f "$path" ]] && cmp -s "$new" "$path" && [[ "$(stat -c '%a %U:%G' "$path")" == "${mode#0} $owner" ]]; then
    report unchanged "$label ($path)"
    return 1
  fi
  report "$(changed_or_would)" "$label ($path)"
  if [[ $DRY_RUN -eq 0 ]]; then
    mkdir -p "$(dirname "$path")"
    install -m "$mode" -o "${owner%%:*}" -g "${owner##*:}" "$new" "$path.dol-tmp"
    mv -f "$path.dol-tmp" "$path"
  fi
  return 0
}

# ensure_dir <path> <mode> <owner:group> <label>
ensure_dir() {
  local path="$1" mode="$2" owner="$3" label="$4"
  if [[ -d "$path" && "$(stat -c '%a %U:%G' "$path")" == "${mode#0} $owner" ]]; then
    report unchanged "$label ($path)"
    return
  fi
  report "$(changed_or_would)" "$label ($path)"
  run install -d -m "$mode" -o "${owner%%:*}" -g "${owner##*:}" "$path"
}

have() { command -v "$1" >/dev/null 2>&1; }
pkg_installed() { dpkg-query -W -f='${Status}' "$1" 2>/dev/null | grep -q "install ok installed"; }

# --- environment detection ---
# shellcheck source=/dev/null
. /etc/os-release 2>/dev/null || die "cannot read /etc/os-release"
case "${ID:-}" in
  ubuntu|debian) DISTRO="$ID" ;;
  *) case "${ID_LIKE:-}" in
       *ubuntu*) DISTRO=ubuntu ;; *debian*) DISTRO=debian ;;
       *) die "unsupported distribution ${ID:-unknown}: Ubuntu or Debian required" ;;
     esac ;;
esac
CODENAME="${UBUNTU_CODENAME:-${VERSION_CODENAME:-}}"
[[ -n "$CODENAME" ]] || die "cannot determine the release codename from /etc/os-release"
DEB_ARCH="$(dpkg --print-architecture)"
case "$DEB_ARCH" in amd64|arm64) ;; *) die "unsupported architecture $DEB_ARCH (amd64 or arm64 required)" ;; esac

IS_WSL=0
if [[ -e /proc/sys/fs/binfmt_misc/WSLInterop ]] || grep -qi microsoft /proc/version 2>/dev/null; then IS_WSL=1; fi
if [[ "$SSHD_MODE" == auto ]]; then
  if [[ $IS_WSL -eq 1 || $CONTAINER_MODE -eq 1 ]]; then SSHD_MODE=only-2222; else SSHD_MODE=alongside; fi
fi
if [[ $IS_WSL -eq 1 && "$SSHD_MODE" == alongside ]]; then
  die "--sshd-mode alongside is not allowed on WSL: in mirrored networking Linux and Windows share ports, and port 22 belongs to Windows OpenSSH"
fi
[[ -n "$NODE_NAME" ]] || NODE_NAME="$(hostname -s | tr '[:upper:]_' '[:lower:]-' | tr -cd 'a-z0-9-' | cut -c1-32)"
[[ "$NODE_NAME" =~ ^[a-z0-9]([a-z0-9-]{0,30}[a-z0-9])?$ ]] || die "node name '$NODE_NAME' is invalid: use 1-32 lowercase letters, digits or '-' (--node-name)"

echo "docker-offload-lan install: $PRETTY_NAME ($DEB_ARCH), wsl=$IS_WSL, container-mode=$CONTAINER_MODE, sshd-mode=$SSHD_MODE, dry-run=$DRY_RUN"

# --- 1. systemd ---
step_systemd() {
  if [[ $CONTAINER_MODE -eq 1 ]]; then report skipped "systemd (container mode)"; return; fi
  if [[ -d /run/systemd/system ]]; then report unchanged "systemd is running"; return; fi
  if [[ $IS_WSL -eq 0 ]]; then die "systemd is not running; this installer needs systemd (or --container-mode)"; fi
  # WSL without systemd: enable it in /etc/wsl.conf ([boot] systemd=true).
  local cur="" new
  [[ -f /etc/wsl.conf ]] && cur="$(cat /etc/wsl.conf)"
  new="$(printf '%s\n' "$cur" | awk '
    BEGIN { insec=0; done=0 }
    /^[[:space:]]*\[/ { if (insec && !done) { print "systemd=true"; done=1 }
                        insec = ($0 ~ /^[[:space:]]*\[boot\][[:space:]]*$/) }
    insec && /^[[:space:]]*systemd[[:space:]]*=/ { if (!done) { print "systemd=true"; done=1 }; next }
    { print }
    END { if (insec && !done) { print "systemd=true"; done=1 }
          if (!done) { print ""; print "[boot]"; print "systemd=true" } }' | sed -e '/./,$!d')"
  put_file /etc/wsl.conf 0644 root:root "WSL: enable systemd in /etc/wsl.conf" <<<"$new" || true
  cat <<EOF

systemd is not running in this WSL distro yet. It has been enabled in /etc/wsl.conf.
Next:
  1. In Windows PowerShell run:   wsl --shutdown
  2. Re-open the distro and re-run this script with the same flags.
EOF
  exit 0
}

# --- 2. base packages ---
step_packages() {
  local want=(ca-certificates curl jq rsync openssh-server iproute2 util-linux) missing=()
  for p in "${want[@]}"; do pkg_installed "$p" || missing+=("$p"); done
  if [[ ${#missing[@]} -eq 0 ]]; then report unchanged "base packages (${want[*]})"; return; fi
  report "$(changed_or_would)" "base packages: install ${missing[*]}"
  run apt-get update -qq
  run apt-get install -y -qq --no-install-recommends "${missing[@]}"
}

# --- 3. Docker Engine from Docker's apt repository (deb822) ---
step_docker() {
  local keyring=/etc/apt/keyrings/docker.asc sources=/etc/apt/sources.list.d/docker.sources
  if [[ -s "$keyring" ]]; then
    report unchanged "Docker apt key ($keyring)"
  else
    report "$(changed_or_would)" "Docker apt key ($keyring)"
    if [[ $DRY_RUN -eq 0 ]]; then
      install -m 0755 -d /etc/apt/keyrings
      curl -fsSL "https://download.docker.com/linux/${DISTRO}/gpg" -o "$keyring"
      chmod a+r "$keyring"
    fi
  fi
  put_file "$sources" 0644 root:root "Docker apt repository" <<EOF || true
Types: deb
URIs: https://download.docker.com/linux/${DISTRO}
Suites: ${CODENAME}
Components: stable
Architectures: ${DEB_ARCH}
Signed-By: ${keyring}
EOF

  local pkgs=(docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin) missing=()
  for p in "${pkgs[@]}"; do pkg_installed "$p" || missing+=("$p"); done
  if [[ ${#missing[@]} -eq 0 ]]; then report unchanged "Docker Engine packages"; return; fi
  local conflicts=() c
  for c in docker.io docker-compose docker-compose-v2 docker-doc podman-docker containerd runc; do
    pkg_installed "$c" && conflicts+=("$c")
  done
  if [[ ${#conflicts[@]} -gt 0 ]]; then
    report "$(changed_or_would)" "remove conflicting packages: ${conflicts[*]}"
    run apt-get remove -y -qq "${conflicts[@]}"
  fi
  report "$(changed_or_would)" "Docker Engine packages: install ${missing[*]}"
  run apt-get update -qq
  run apt-get install -y -qq "${missing[@]}"
}

# --- 4. daemon.json (merge, jq) ---
DOCKER_RESTART_NEEDED=0
step_daemon_json() {
  local frag merged
  frag="$(jq -n --arg max "$BUILDKIT_MAX_USED" '{
    "log-driver": "local",
    "log-opts": {"max-size": "20m", "max-file": "5"},
    "builder": {"gc": {"enabled": true, "defaultReservedSpace": "10GB",
                       "defaultMaxUsedSpace": $max, "defaultMinFreeSpace": "20GB"}}}')"
  if [[ -f "$DAEMON_JSON" ]]; then
    jq -e . "$DAEMON_JSON" >/dev/null 2>&1 || die "$DAEMON_JSON is not valid JSON; fix or move it away and re-run"
    # Deep merge: our keys win, every other key is kept.
    merged="$(jq -S --argjson f "$frag" '. * $f' "$DAEMON_JSON")"
    if [[ "$merged" == "$(jq -S . "$DAEMON_JSON")" ]]; then
      report unchanged "Docker daemon.json ($DAEMON_JSON)"; return
    fi
    if [[ $DRY_RUN -eq 0 ]]; then cp -p "$DAEMON_JSON" "$DAEMON_JSON.bak-dol-$(date +%Y%m%d%H%M%S)"; fi
  else
    merged="$(jq -S . <<<"$frag")"
  fi
  # Here-strings, not pipes: put_file must run in this shell to count changes.
  if put_file "$DAEMON_JSON" 0644 root:root "Docker daemon.json (log-driver local, BuildKit GC max $BUILDKIT_MAX_USED)" <<<"$merged"; then
    DOCKER_RESTART_NEEDED=1
  fi
}

step_docker_service() {
  if [[ $CONTAINER_MODE -eq 1 ]]; then report skipped "docker.service (container mode: the entrypoint starts dockerd)"; return; fi
  if [[ $DOCKER_RESTART_NEEDED -eq 1 ]]; then
    report "$(changed_or_would)" "restart docker.service to apply daemon.json"
    run systemctl restart docker.service
  fi
  if systemctl is-enabled --quiet docker.service 2>/dev/null && systemctl is-active --quiet docker.service; then
    report unchanged "docker.service enabled and running"
  else
    report "$(changed_or_would)" "enable and start docker.service"
    run systemctl enable --now docker.service
  fi
}

# --- 5. user and group ---
step_user() {
  if getent group "$DOL_GROUP" >/dev/null; then report unchanged "group $DOL_GROUP"; else
    report "$(changed_or_would)" "group $DOL_GROUP"; run groupadd --system "$DOL_GROUP"; fi
  getent group docker >/dev/null || { report "$(changed_or_would)" "group docker"; run groupadd --system docker; }
  if id -u "$DOL_USER" >/dev/null 2>&1; then report unchanged "user $DOL_USER"; else
    report "$(changed_or_would)" "user $DOL_USER"
    run useradd --create-home --shell /bin/bash --gid "$DOL_GROUP" --comment "docker-offload-lan" "$DOL_USER"
  fi
  # '*' instead of the default '!' (locked): key-only, but not "locked" for sshd.
  if [[ "$(getent shadow "$DOL_USER" 2>/dev/null | cut -d: -f2)" == "*" ]]; then report unchanged "user $DOL_USER has no password (key-only)"; else
    report "$(changed_or_would)" "user $DOL_USER: no password, key-only login"; run usermod -p '*' "$DOL_USER"; fi
  if id -nG "$DOL_USER" 2>/dev/null | tr ' ' '\n' | grep -qx docker; then report unchanged "user $DOL_USER in group docker"; else
    report "$(changed_or_would)" "add $DOL_USER to group docker (root-equivalent, see docs/security.md)"; run usermod -aG docker "$DOL_USER"; fi
}

# --- 6. authorized_keys ---
valid_pubkey() { [[ "$1" =~ ^(ssh-ed25519|ssh-rsa|ecdsa-sha2-nistp(256|384|521)|sk-ssh-ed25519@openssh.com|sk-ecdsa-sha2-nistp256@openssh.com)\ [A-Za-z0-9+/=]+(\ .*)?$ ]]; }
step_authorized_keys() {
  local home ak
  home="$(getent passwd "$DOL_USER" | cut -d: -f6)"; home="${home:-/home/$DOL_USER}"
  ak="$home/.ssh/authorized_keys"
  ensure_dir "$home/.ssh" 0700 "$DOL_USER:$DOL_GROUP" "ssh dir of $DOL_USER"
  local keys=() k line
  for k in "${PUBKEYS[@]+"${PUBKEYS[@]}"}"; do
    if [[ -f "$k" ]]; then
      while IFS= read -r line || [[ -n "$line" ]]; do [[ -n "$line" && "$line" != \#* ]] && keys+=("$line"); done <"$k"
    else keys+=("$k"); fi
  done
  local cur=""
  [[ -f "$ak" ]] && cur="$(cat "$ak")"
  local new="$cur" added=0 blob
  for k in "${keys[@]+"${keys[@]}"}"; do
    valid_pubkey "$k" || die "--pubkey: not an OpenSSH public key: ${k:0:40}…"
    blob="$(awk '{print $2}' <<<"$k")"
    if ! grep -qF -- "$blob" <<<"$cur"; then new="${new:+$new$'\n'}$k"; added=$((added + 1)); fi
  done
  if [[ $added -eq 0 && -f "$ak" ]]; then
    put_file "$ak" 0600 "$DOL_USER:$DOL_GROUP" "authorized_keys of $DOL_USER" <<<"$cur" || true
  elif [[ -n "$new" ]]; then
    put_file "$ak" 0600 "$DOL_USER:$DOL_GROUP" "authorized_keys of $DOL_USER (+$added key(s))" <<<"$new" || true
  fi
  if [[ -z "$new" ]]; then
    echo "  WARNING: no SSH key is authorized for $DOL_USER yet. Re-run with --pubkey <your Mac's ~/.ssh/id_ed25519.pub>." >&2
  fi
}

# --- 7. sshd on 2222 ---
step_sshd() {
  local ports="Port ${SSH_PORT}"
  [[ "$SSHD_MODE" == alongside ]] && ports="Port 22"$'\n'"Port ${SSH_PORT}"
  local reload=0
  if put_file "$SSHD_DROPIN" 0644 root:root "sshd drop-in (port ${SSH_PORT}, key-only, AllowUsers ${DOL_USER}, MaxSessions 64)" <<EOF
# Managed by docker-offload-lan install.sh. Re-running the installer rewrites this file.
# Docker-over-SSH and \`dol node dial-stdio\` multiplex many sessions over one
# ControlMaster connection; the default MaxSessions (10) is too low.
${ports}

Match LocalPort ${SSH_PORT}
    AllowUsers ${DOL_USER}
    AuthenticationMethods publickey
    PubkeyAuthentication yes
    PasswordAuthentication no
    KbdInteractiveAuthentication no
    PermitRootLogin no
    MaxSessions 64
    AllowTcpForwarding local
    X11Forwarding no
    AllowAgentForwarding no
Match all
EOF
  then reload=1; fi
  if grep -Eq '^[[:space:]]*Include[[:space:]]+/etc/ssh/sshd_config\.d/\*\.conf' /etc/ssh/sshd_config 2>/dev/null; then
    report unchanged "sshd_config includes sshd_config.d"
  else
    report "$(changed_or_would)" "sshd_config: add Include /etc/ssh/sshd_config.d/*.conf at the top"
    if [[ $DRY_RUN -eq 0 ]]; then
      cp -p /etc/ssh/sshd_config "/etc/ssh/sshd_config.bak-dol-$(date +%Y%m%d%H%M%S)"
      { echo "Include /etc/ssh/sshd_config.d/*.conf"; cat /etc/ssh/sshd_config; } >"$tmpdir/sshd_config"
      install -m 0644 "$tmpdir/sshd_config" /etc/ssh/sshd_config
    fi
    reload=1
  fi
  if [[ "$SSHD_MODE" == only-2222 ]] && grep -Eq '^[[:space:]]*Port[[:space:]]+22[[:space:]]*$' /etc/ssh/sshd_config 2>/dev/null; then
    echo "  WARNING: /etc/ssh/sshd_config sets 'Port 22' explicitly, so sshd also listens on 22." >&2
  fi
  if [[ $DRY_RUN -eq 0 ]]; then
    mkdir -p /run/sshd
    [[ -n "$(ls /etc/ssh/ssh_host_*_key 2>/dev/null)" ]] || ssh-keygen -A >/dev/null
    sshd -t || die "sshd configuration test failed (sshd -t); see the messages above"
  fi
  if [[ $CONTAINER_MODE -eq 1 ]]; then report skipped "ssh service (container mode: the entrypoint starts sshd)"; return; fi
  # Ubuntu >= 22.10 uses socket activation (ssh.socket), whose listen ports come
  # from a generator, not from sshd. Use the plain service so Port is honoured.
  if systemctl list-unit-files ssh.socket >/dev/null 2>&1 && systemctl is-enabled --quiet ssh.socket 2>/dev/null; then
    report "$(changed_or_would)" "switch sshd from ssh.socket to ssh.service (ADR-0030)"
    run systemctl disable --now ssh.socket
    run rm -f /etc/systemd/system/ssh.service.d/00-socket.conf /etc/systemd/system/ssh.socket.d/addresses.conf
    run systemctl daemon-reload
    reload=1
  fi
  if ! systemctl is-enabled --quiet ssh.service 2>/dev/null; then
    report "$(changed_or_would)" "enable ssh.service"; run systemctl enable ssh.service
  fi
  if [[ $reload -eq 1 ]] || ! systemctl is-active --quiet ssh.service; then
    report "$(changed_or_would)" "restart ssh.service"; run systemctl restart ssh.service
  else
    report unchanged "ssh.service running"
  fi
}

# --- 8. kind, kubectl ---
download_verified() { # download_verified <url> <sha256> <dest>
  local url="$1" sum="$2" dest="$3" f="$tmpdir/dl.$RANDOM"
  curl -fsSL --retry 3 -o "$f" "$url" || die "download failed: $url"
  echo "$sum  $f" | sha256sum --check --status || die "sha256 mismatch for $url (expected $sum); refusing to install"
  install -m 0755 -o root -g root "$f" "$dest"
}
step_kind() {
  local cur=""
  [[ -x "$BIN_DIR/kind" ]] && cur="$("$BIN_DIR/kind" version 2>/dev/null | awk '{print $2}')"
  if [[ "$cur" == "$KIND_VERSION" ]]; then report unchanged "kind $KIND_VERSION"; return; fi
  report "$(changed_or_would)" "kind ${cur:-(none)} -> $KIND_VERSION (sha256 verified)"
  [[ $DRY_RUN -eq 1 ]] || download_verified "https://kind.sigs.k8s.io/dl/${KIND_VERSION}/kind-linux-${DEB_ARCH}" "$(pinned_sha256 kind "$DEB_ARCH")" "$BIN_DIR/kind"
}
step_kubectl() {
  local cur=""
  [[ -x "$BIN_DIR/kubectl" ]] && cur="$("$BIN_DIR/kubectl" version --client -o json 2>/dev/null | jq -r '.clientVersion.gitVersion // empty' || true)"
  if [[ "$cur" == "$KUBECTL_VERSION" ]]; then report unchanged "kubectl $KUBECTL_VERSION"; return; fi
  report "$(changed_or_would)" "kubectl ${cur:-(none)} -> $KUBECTL_VERSION (sha256 verified)"
  [[ $DRY_RUN -eq 1 ]] || download_verified "https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/linux/${DEB_ARCH}/kubectl" "$(pinned_sha256 kubectl "$DEB_ARCH")" "$BIN_DIR/kubectl"
}

# --- 9. dol binary ---
step_dol() {
  local dest="$BIN_DIR/dol"
  if [[ $NO_DOL -eq 1 ]]; then report skipped "dol binary (--no-dol)"; return; fi
  if [[ -n "$DOL_BINARY" ]]; then
    if [[ -x "$dest" ]] && cmp -s "$DOL_BINARY" "$dest"; then report unchanged "dol binary ($dest)"; return; fi
    report "$(changed_or_would)" "dol binary from $DOL_BINARY ($dest)"
    run install -m 0755 -o root -g root "$DOL_BINARY" "$dest"
    return
  fi
  if [[ -x "$dest" && -z "$DOL_VERSION" ]]; then report unchanged "dol binary ($dest, $("$dest" --version 2>/dev/null | head -1))"; return; fi
  local ver="$DOL_VERSION"
  if [[ -z "$ver" || "$ver" == latest ]]; then
    ver="$(curl -fsSL "https://api.github.com/repos/${DOL_REPO}/releases/latest" | jq -r .tag_name)" \
      || die "cannot determine the latest dol release; pass --dol-binary <path>"
  fi
  if [[ -x "$dest" ]] && "$dest" --version 2>/dev/null | grep -qF "${ver#v}"; then report unchanged "dol $ver ($dest)"; return; fi
  report "$(changed_or_would)" "dol $ver from GitHub releases ($dest)"
  [[ $DRY_RUN -eq 1 ]] && return
  local base="https://github.com/${DOL_REPO}/releases/download/${ver}" tgz="dol_${ver#v}_linux_${DEB_ARCH}.tar.gz"
  curl -fsSL --retry 3 -o "$tmpdir/$tgz" "$base/$tgz" || die "download failed: $base/$tgz"
  curl -fsSL --retry 3 -o "$tmpdir/checksums.txt" "$base/checksums.txt" || die "download failed: $base/checksums.txt"
  (cd "$tmpdir" && grep -F " $tgz" checksums.txt | sha256sum --check --status) || die "sha256 mismatch for $tgz"
  tar -xzf "$tmpdir/$tgz" -C "$tmpdir" dol
  install -m 0755 -o root -g root "$tmpdir/dol" "$dest"
}

# --- 10. directories and node.yaml ---
step_dirs() {
  ensure_dir "$CONF_DIR" 0755 root:root "config dir"
  ensure_dir "$SYNC_ROOT" 0750 "$DOL_USER:$DOL_GROUP" "workspace sync root"
  if [[ $CONTAINER_MODE -eq 1 ]]; then
    # systemd's StateDirectory=/RuntimeDirectory= do this outside containers.
    ensure_dir "$STATE_DIR" 0750 "$DOL_USER:$DOL_GROUP" "state dir"
    ensure_dir "$RUN_DIR" 0750 "$DOL_USER:$DOL_GROUP" "runtime dir"
  fi
}

step_node_yaml() {
  if [[ -f "$NODE_YAML" ]]; then report unchanged "node config ($NODE_YAML; never overwritten, edit it by hand)"; return; fi
  report "$(changed_or_would)" "node config ($NODE_YAML)"
  [[ $DRY_RUN -eq 1 ]] && return
  # Prefer the binary's own generator; fall back to a template when the
  # installed dol does not implement `node init` (yet).
  if "$BIN_DIR/dol" node init --name "$NODE_NAME" >"$tmpdir/node-init.log" 2>&1 && [[ -f "$NODE_YAML" ]]; then
    echo "  (written by \`dol node init\`)"
    return
  fi
  install -m 0644 -o root -g root /dev/stdin "$NODE_YAML" <<EOF
# docker-offload-lan node config. Schema: schemas/node-config.schema.json
version: 1
name: ${NODE_NAME}
socket: ${RUN_DIR}/node.sock
data_dir: ${STATE_DIR}
sync_root: ${SYNC_ROOT}
capacity:
  provider: auto
  reserve_memory_gb: 4
  min_free_disk_gb: 30
  windows_memory_check: true
leases:
  heartbeat_grace_s: 600
  max_ttl_s: 86400
  max_active: 32
reaper:
  interval_s: 60
  reap_unlabeled_after_s: 0
kind:
  binary: ${BIN_DIR}/kind
  port_range: [16443, 16999]
  access_mode: tunnel
EOF
}

# --- 11. systemd unit ---
step_unit() {
  if [[ $CONTAINER_MODE -eq 1 ]]; then report skipped "dol-node.service (container mode)"; return; fi
  local src="$SCRIPT_DIR/dol-node.service" restart=0
  [[ -f "$src" ]] || die "missing $src (run install.sh from a checkout or release archive)"
  if put_file "$UNIT_PATH" 0644 root:root "systemd unit dol-node.service" <"$src"; then
    run systemctl daemon-reload; restart=1
  fi
  if ! systemctl is-enabled --quiet dol-node.service 2>/dev/null; then
    report "$(changed_or_would)" "enable dol-node.service"; run systemctl enable dol-node.service; restart=1
  fi
  if [[ $restart -eq 1 ]] || ! systemctl is-active --quiet dol-node.service; then
    report "$(changed_or_would)" "(re)start dol-node.service"
    run systemctl restart dol-node.service || echo "  WARNING: dol-node.service failed to start: journalctl -u dol-node -n 50" >&2
  else
    report unchanged "dol-node.service running"
  fi
}

step_systemd
step_packages
step_docker
step_daemon_json
step_docker_service
step_user
step_authorized_keys
step_sshd
step_kind
step_kubectl
step_dol
step_dirs
step_node_yaml
step_unit

echo
if [[ $CHANGES -eq 0 ]]; then
  echo "install.sh: done, nothing changed (all steps unchanged)."
else
  echo "install.sh: done, $CHANGES change(s)$([[ $DRY_RUN -eq 1 ]] && echo ' would be made (dry run)')."
fi

if [[ $SKIP_DOCTOR -eq 0 && $DRY_RUN -eq 0 && -x "$BIN_DIR/dol" ]]; then
  echo
  echo "Running: dol doctor --node"
  if ! "$BIN_DIR/dol" doctor --node; then
    echo "install.sh: doctor reported problems (see above and docs/troubleshooting.md). The installation itself completed." >&2
  fi
fi

if [[ $IS_WSL -eq 1 || $CONTAINER_MODE -eq 0 ]]; then
  cat <<EOF

Next, on your Mac:
  dol init --name ${NODE_NAME} --host <this machine's LAN IP>
(the Windows setup script printed the LAN IP; sshd listens on port ${SSH_PORT}, user ${DOL_USER}).
EOF
fi
