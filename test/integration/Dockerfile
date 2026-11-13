# "Fake node" for integration tests: Ubuntu 24.04 set up by the real
# deploy/linux/install.sh (--container-mode), with Docker-in-Docker, sshd on
# 2222 and the dol agent. Built by test/integration/harness from a temporary
# context containing: Dockerfile, entrypoint.sh, install.sh, dol-node.service, dol.
# Run privileged (dockerd needs it).
FROM ubuntu:24.04

# 1. The heavy, cacheable part: packages, Docker Engine, kind, kubectl, user,
#    sshd config. The dol binary changes on every build, so it comes later.
COPY install.sh dol-node.service /opt/dol/deploy/
RUN /opt/dol/deploy/install.sh --container-mode --no-dol --node-name it --skip-doctor \
 && rm -rf /var/lib/apt/lists/*

# pam_loginuid cannot always set the login uid inside containers; it is
# irrelevant here. (Test image only; real nodes keep the distro default.)
RUN sed -i 's/^session\s\+required\s\+pam_loginuid.so/session optional pam_loginuid.so/' /etc/pam.d/sshd

# 2. The dol binary (cross-compiled by the harness). Second install.sh run:
#    everything else must report "unchanged".
COPY dol /opt/dol/dol
RUN /opt/dol/deploy/install.sh --container-mode --dol-binary /opt/dol/dol --node-name it --skip-doctor

COPY entrypoint.sh /usr/local/bin/dol-it-entrypoint
VOLUME /var/lib/docker
EXPOSE 2222
ENTRYPOINT ["/usr/local/bin/dol-it-entrypoint"]
