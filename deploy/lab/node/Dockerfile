# A "VM" for the Ansible lab: Ubuntu with systemd as PID 1, SSH and Python, like a fresh cloud server.
# Runs privileged (k3s and Docker need cgroups, iptables and overlay mounts). Lab only, never production.
FROM ubuntu:24.04
ENV container=docker DEBIAN_FRONTEND=noninteractive
RUN apt-get update \
 && apt-get install -y --no-install-recommends systemd systemd-sysv dbus openssh-server sudo python3 python3-apt \
      ca-certificates curl iproute2 iptables kmod gnupg lsb-release \
 && apt-get clean && rm -rf /var/lib/apt/lists/* \
 && systemctl enable ssh \
 && systemctl mask systemd-logind getty.target console-getty.service \
 && mkdir -p /run/sshd
# The lab's bootstrap account; the common role creates the real deploy user and locks root login.
RUN useradd -m -s /bin/bash -G sudo ubuntu-lab \
 && echo 'ubuntu-lab ALL=(ALL) NOPASSWD:ALL' > /etc/sudoers.d/ubuntu-lab && chmod 0440 /etc/sudoers.d/ubuntu-lab \
 && mkdir -p /home/ubuntu-lab/.ssh && chmod 700 /home/ubuntu-lab/.ssh && chown -R ubuntu-lab: /home/ubuntu-lab/.ssh
STOPSIGNAL SIGRTMIN+3
CMD ["/sbin/init"]
