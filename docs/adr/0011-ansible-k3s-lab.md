# ADR 0011: Ansible + k3s on a container lab

## Status
Accepted (Phase 8)

## Context
The spec asks for 2–3 machines provisioned by Ansible (hardening, Docker, Oracle, Redis/Kafka, monitoring, k3s server +
agent, application deploy), secrets in Ansible Vault and a rolling deploy with `serial: 1`. The development machine is
Windows 11 Home with Docker Desktop: no Multipass, Vagrant or VirtualBox, and Ansible does not run natively on Windows.

## Decision
**Three privileged Ubuntu 24.04 containers act as the VMs** (`deploy/lab`): systemd as PID 1, SSH, Python, a bootstrap
account with a key (what cloud-init gives a fresh server). A fourth container is the **Ansible controller**. The
playbooks do not know they run on containers; the same inventory groups work for real servers.

| Node | Groups | Roles |
|---|---|---|
| lab-data 172.30.0.10 | `data` | common, docker, oracle, redis_kafka, monitoring |
| lab-k3s-server 172.30.0.11 | `k3s`, `k3s_server` | common, k3s (server), app_deploy |
| lab-k3s-agent 172.30.0.12 | `k3s`, `k3s_agent` | common, k3s (agent), app_deploy |

- **common**: `deploy` user (sudo, SSH key only), `PermitRootLogin no`, no password login, node_exporter as a systemd
  service, ufw: deny incoming except SSH and the platform subnet; on k3s nodes also the pod and service CIDRs and the
  CNI interfaces (`cni0`, `flannel.1`) so DNS, API calls and kubelet probes reach the pods. Routed traffic allowed.
  `common_firewall: false` turns the host firewall off for environments that filter with a security group instead.
- **oracle** reuses `db/migrate.sh` and `db/tune-redo.sh` unchanged: the data node runs the same migrations as a dev
  machine and CI. **redis_kafka**: Kafka advertises the data node IP (pods cannot resolve the node's container names).
- **k3s**: pinned version, token from the vault, server first, agents join it, wait until every node is Ready.
- **app_deploy**: images are built and saved by `make lab-images` (tag = git commit), copied and imported into
  containerd on every node (`imagePullPolicy: Never`, no registry credentials in the lab). Manifests are Jinja templates in
  `deploy/k8s/`; Oracle, Redis and Kafka become in-cluster names through Services without selector + EndpointSlices.
  Pods run as non-root with a read-only root filesystem and dropped capabilities.
- **Secrets**: `ansible/group_vars/all/vault.yml` (ansible-vault, random values). The vault password is read from
  `FLASHSALE_VAULT_PASSWORD` by `vault-pass.sh`; the Kubernetes Secret is piped from vault variables to `kubectl apply`,
  never written to a file. The lab password is a dev value in `.env.example`, like every other dev secret there; a real
  environment has its own vault file and its password in a secret store.
- **Rolling**: application deploys roll inside Kubernetes (`maxUnavailable: 0`, readiness on `/readyz`, replicas
  spread over the nodes). Node maintenance is `rolling-update.yml` with `serial: 1`: drain → security updates + k3s at
  the pinned version → Ready → uncordon → every Deployment Available, then the next node.

**Measured** (`make lab-rolling`, `deploy/lab/probe.py` calls the gateway NodePort of both nodes every second):

| | node .11 (server) | node .12 (agent) |
|---|---|---|
| first version | 37 of 40 probes failed, two outages of about 18 s on **both** nodes | same |
| with PodDisruptionBudgets + 2 CoreDNS replicas | 4 of 67 failed, longest 3 s (its own k3s restart) | 1 of 67 failed |

The first version lost the whole platform during each drain: CoreDNS had one replica (k3s default), so while its node
was drained no service name resolved, and the drain evicted every replica of a node without waiting for a replacement.
The k3s role now runs 2 CoreDNS replicas with a PDB, every Deployment has a PDB (`minAvailable: 1`, so notification
runs 2 consumers), and the remaining seconds are the restarting node's own NodePort, which a load balancer with health
checks in front of the nodes would skip.

`site.yml` is idempotent: a second run reports `changed=0` on every host (CI checks it). Two fixes were needed:
EndpointSlice endpoints need an explicit `conditions: { ready: true }`, and `kubectl apply` reports every
PodDisruptionBudget as "configured" even when nothing differs, so the roles detect changes with `kubectl diff` and
apply only on a real difference.

## Consequences
- Lab-specific workarounds, all in `deploy/lab`, each found by a failing run:
  - `/var/lib/rancher/k3s`, `/var/lib/docker` and `/var/lib/containerd` live on Docker volumes: overlayfs cannot be
    stacked on overlayfs (k3s: `"overlayfs" snapshotter cannot be enabled`; Docker: `failed to mount ... overlay`).
  - Each node has its **own cgroup namespace** (`cgroup: private`). With the host's, both kubelets managed the same
    `kubepods` tree and killed each other's pods: every pod, CoreDNS included, ended in CrashLoopBackOff after a clean
    SIGTERM, and pods could not resolve `redis` or `kafka`. (We first suspected ufw; turning it off seemed to help only
    because CoreDNS happened to restart on the other node.)
  - `/etc/rancher` is a volume too: a re-created container lost `/etc/rancher/node/password` and the server rejected
    the agent ("Node password rejected"); the fix for an existing cluster is deleting the `<node>.node-password.k3s`
    secret. On a VM, `/etc` simply survives.
- Bind-mounting a single file is fragile: when the source is missing, Docker creates a directory there. The monitoring
  role mounts the whole config directory and removes such a stray directory.
- The firewall of a container is real (its own network namespace), but the kernel is shared with Docker Desktop.
- The lab needs about 5 GB of the Docker VM: stop the compose stack (`make down`) before `make lab-up`.
- One data node: Oracle, Redis and Kafka are single points of failure here, as in the compose stack.
