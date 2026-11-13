#!/usr/bin/env bash
# Entrypoint of the integration "fake node": starts dockerd, applies the
# per-test ssh key with install.sh, then starts sshd and `dol node serve`
# (tolerating a dol build without the node agent). Touches /run/dol-it-ready
# when sshd is up.
set -euo pipefail
log() { echo "[entrypoint] $*"; }

# cgroup v2 nesting, as in the official docker:dind image: move ourselves out
# of the root cgroup so dockerd can enable controllers for its children.
if [[ -f /sys/fs/cgroup/cgroup.controllers ]]; then
  mkdir -p /sys/fs/cgroup/init
  xargs -rn1 </sys/fs/cgroup/cgroup.procs >/sys/fs/cgroup/init/cgroup.procs 2>/dev/null || true
  sed -e 's/ / +/g' -e 's/^/+/' </sys/fs/cgroup/cgroup.controllers >/sys/fs/cgroup/cgroup.subtree_control 2>/dev/null || true
fi

log "starting dockerd"
dockerd >/var/log/dockerd.log 2>&1 &
for _ in $(seq 1 120); do docker info >/dev/null 2>&1 && break; sleep 0.5; done
if docker info >/dev/null 2>&1; then log "dockerd is up"; else log "dockerd did not come up:"; tail -40 /var/log/dockerd.log; fi

args=(--container-mode --dol-binary /opt/dol/dol --node-name it --skip-doctor)
if [[ -n "${DOL_PUBKEY:-}" ]]; then args+=(--pubkey "$DOL_PUBKEY"); fi
log "install.sh ${args[*]}"
if ! /opt/dol/deploy/install.sh "${args[@]}" >/var/log/dol-install.log 2>&1; then
  log "install.sh failed:"; cat /var/log/dol-install.log; exit 1
fi

mkdir -p /run/sshd
install -d -m 0750 -o dol -g dol /run/docker-offload-lan /var/lib/docker-offload-lan
log "starting sshd on 2222"
/usr/sbin/sshd -E /var/log/sshd.log

# The node agent, as user dol with groups dol+docker (what dol-node.service does).
(
  while true; do
    setpriv --reuid=dol --regid=dol --init-groups \
      env HOME=/var/lib/docker-offload-lan DOCKER_CONFIG=/var/lib/docker-offload-lan/.docker \
      /usr/local/bin/dol node serve >>/var/log/dol-node.log 2>&1 && rc=0 || rc=$?
    echo "[entrypoint] dol node serve exited with $rc" >>/var/log/dol-node.log
    if grep -q "not implemented" /var/log/dol-node.log; then
      echo "[entrypoint] this dol build has no node agent yet; not restarting" >>/var/log/dol-node.log
      break
    fi
    sleep 2
  done
) &

touch /run/dol-it-ready
log "ready"
wait
