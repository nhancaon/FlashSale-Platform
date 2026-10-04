# Ansible controller for the lab (Ansible does not run natively on Windows).
FROM python:3.13-slim
RUN apt-get update && apt-get install -y --no-install-recommends openssh-client git \
 && rm -rf /var/lib/apt/lists/* \
 && pip install --no-cache-dir "ansible-core>=2.19,<2.20" ansible-lint
COPY requirements.yml /tmp/requirements.yml
RUN ansible-galaxy collection install -r /tmp/requirements.yml -p /usr/share/ansible/collections
