#!/usr/bin/env bash
# Ansible lab driver (deploy/lab): three privileged "VM" containers + an Ansible controller container.
#   bash scripts/lab.sh up | images | ansible <playbook> [args] | check | rolling | down | destroy
set -euo pipefail
export MSYS_NO_PATHCONV=1
cd "$(dirname "$0")/.."
[ -f .env ] || cp .env.example .env
COMPOSE=(docker compose --env-file .env -f deploy/lab/docker-compose.yml)
NODES=(lab-data lab-k3s-server lab-k3s-agent)
SERVICES=(gateway order inventory-go inventory-java outbox-worker notification)
VERSION="${APP_VERSION:-$(git rev-parse --short HEAD)}"

up() {
  mkdir -p deploy/lab/.ssh deploy/lab/images
  [ -f deploy/lab/.ssh/id_ed25519 ] || ssh-keygen -q -t ed25519 -N '' -C flashsale-lab -f deploy/lab/.ssh/id_ed25519
  "${COMPOSE[@]}" up -d --build
  local pub; pub=$(cat deploy/lab/.ssh/id_ed25519.pub)
  for n in "${NODES[@]}"; do
    for _ in $(seq 1 30); do
      state=$(docker exec "$n" systemctl is-system-running 2>/dev/null || true)
      case "$state" in running|degraded) break ;; esac
      sleep 1
    done
    # what cloud-init does on a real server: the operator's key for the bootstrap account
    docker exec "$n" sh -c "grep -qF '$pub' /home/ubuntu-lab/.ssh/authorized_keys 2>/dev/null || echo '$pub' >> /home/ubuntu-lab/.ssh/authorized_keys; chown ubuntu-lab: /home/ubuntu-lab/.ssh/authorized_keys; chmod 600 /home/ubuntu-lab/.ssh/authorized_keys"
    echo "$n: $state"
  done
  # ssh refuses keys readable by others; the bind mount from Windows is 0777, so the controller keeps a private copy
  docker exec lab-controller sh -c 'mkdir -p /root/.ssh-lab-rw && cp /root/.ssh-lab/* /root/.ssh-lab-rw/ && chmod 600 /root/.ssh-lab-rw/id_ed25519'
}

images() { # tag the service images with the commit and save them for Ansible (role app_deploy)
  docker compose --env-file .env -f deploy/compose/docker-compose.yml -f deploy/compose/docker-compose.apps.yml \
    --profile inventory-go --profile inventory-java build "${SERVICES[@]}"
  mkdir -p deploy/lab/images
  for s in "${SERVICES[@]}"; do
    docker tag "flashsale/$s:latest" "flashsale/$s:$VERSION"
    [ -f "deploy/lab/images/$s-$VERSION.tar" ] || docker save -o "deploy/lab/images/$s-$VERSION.tar" "flashsale/$s:$VERSION"
    echo "image flashsale/$s:$VERSION"
  done
}

ansible() { # ansible-playbook inside the controller
  docker exec lab-controller ansible-playbook -e app_version="$VERSION" "$@"
}

rolling() { # rolling-update.yml while deploy/lab/probe.py calls the gateway on every node once per second
  docker exec lab-controller sh -c "rm -f /tmp/probe.stop /tmp/probe.out"
  docker exec -d lab-controller sh -c "python3 /repo/deploy/lab/probe.py > /tmp/probe.out 2>&1"
  local rc=0
  ansible rolling-update.yml || rc=$?
  docker exec lab-controller sh -c "touch /tmp/probe.stop; while [ ! -s /tmp/probe.out ]; do sleep 1; done; cat /tmp/probe.out"
  return "$rc"
}

case "${1:-}" in
  up) up ;;
  images) images ;;
  ansible) shift; ansible "$@" ;;
  check) ansible site.yml --check --diff ;;
  rolling) rolling ;;
  down) "${COMPOSE[@]}" stop ;;
  destroy) "${COMPOSE[@]}" down -v ;;
  *) echo "usage: $0 up|images|ansible <playbook> [args]|check|rolling|down|destroy" >&2; exit 2 ;;
esac
