#!/usr/bin/env python3
"""Prepare a dedicated NAS Compose bundle; never invokes Docker or SSH."""
import argparse
import ipaddress
import json
import os
from pathlib import Path
import re
import secrets
import shutil
import sys

IMAGE = re.compile(r"^[a-z0-9][a-z0-9./_-]*@sha256:[a-f0-9]{64}$")
PROJECT = re.compile(r"^[a-z][a-z0-9_-]{0,39}$")


def prepare(output, data_root, image, bind='127.0.0.1', port=8090, project='loopforge-nas'):
    output, data_root = Path(output), Path(data_root)
    if not IMAGE.fullmatch(image):
        raise ValueError('image must be an explicit registry/repository@sha256 digest of the fork build')
    if not PROJECT.fullmatch(project):
        raise ValueError('invalid independent Compose project name')
    address = ipaddress.ip_address(bind)
    if address.is_unspecified or address.is_multicast:
        raise ValueError('bind a specific local interface, not a wildcard or multicast address')
    if not 1024 <= port <= 65535:
        raise ValueError('port must be between 1024 and 65535')
    if not data_root.is_absolute() or '..' in data_root.parts or any(c in str(data_root) for c in '\r\n\x00$'):
        raise ValueError('data root must be an absolute path without traversal or Compose substitutions')
    if str(data_root) in ('/', '/volume1', '/volume2') or len(data_root.parts) < 4:
        raise ValueError('use a dedicated application data subdirectory, never a volume root')
    # Refuse existing targets rather than changing permissions or replacing data.
    if output.exists() or output.is_symlink():
        raise FileExistsError('output already exists; refusing to overwrite')
    password = secrets.token_hex(24)
    root_password = secrets.token_hex(24)
    session = secrets.token_hex(32)
    root = str(data_root)
    environment = {
        'AGENTFLOW_HTTP_PORT': '8080',
        'AGENTFLOW_MYSQL_DSN': 'loopforge:${LOOPFORGE_DB_PASSWORD}@tcp(mysql:3306)/loopforge',
        'AGENTFLOW_SESSION_SECRET': '${AGENTFLOW_SESSION_SECRET:?session secret is required}',
        'AGENTFLOW_AUTH_REQUIRED': 'true',
        'AGENTFLOW_ALLOW_SIGNUP': 'false',
        'AGENTFLOW_SECURE_COOKIES': 'true',
        'AGENTFLOW_LOCAL_SANDBOX': 'docker',
        'AGENTFLOW_LOCAL_EXECUTION': 'false',
        'AGENTFLOW_CLOUD_RUNNER_LABEL': 'nas-executor',
        'AGENTFLOW_CLOUD_RUNNER_PROJECTS': '*',
        'AGENTFLOW_ASSISTANT_SESSION_DIR': '/data/assistant',
        'AGENTFLOW_CLARIFY_SESSION_DIR': '/data/clarify',
        'AGENTFLOW_CONVERSATION_DIR': '/data/conversations',
        'AGENTFLOW_PLATFORM_MCP_URL': 'http://platform:8080',
        'AGENTFLOW_RUNNER_BINARY': '/app/bin/agentflow-runner-{os}-{arch}',
        'AGENTFLOW_GATE_MCP_BINARY': '/app/bin/agentflow-gate-mcp-{os}-{arch}',
        'HOME': '/data/home',
    }
    compose = {
        'name': project,
        'services': {
            'mysql': {
                'image': 'mysql:8.4',
                'environment': {
                    'MYSQL_DATABASE': 'loopforge', 'MYSQL_USER': 'loopforge',
                    'MYSQL_PASSWORD': '${LOOPFORGE_DB_PASSWORD:?database password is required}',
                    'MYSQL_ROOT_PASSWORD': '${LOOPFORGE_DB_ROOT_PASSWORD:?root password is required}',
                },
                'command': ['--character-set-server=utf8mb4', '--collation-server=utf8mb4_unicode_ci'],
                'volumes': [{'type': 'bind', 'source': root + '/mysql', 'target': '/var/lib/mysql'}],
                'healthcheck': {
                    'test': ['CMD-SHELL', 'MYSQL_PWD=$$MYSQL_PASSWORD mysqladmin ping -h 127.0.0.1 -u loopforge --silent'],
                    'interval': '10s', 'timeout': '5s', 'retries': 30,
                },
                'restart': 'unless-stopped',
            },
            'platform': {
                'image': image,
                'ports': [{'target': 8080, 'published': str(port), 'host_ip': str(address), 'protocol': 'tcp'}],
                'environment': environment,
                'command': ['-role', 'all', '-workspaces', '/data/workspaces', '-worker-concurrency', '2'],
                'volumes': [{'type': 'bind', 'source': root + '/platform', 'target': '/data'}],
                'depends_on': {'mysql': {'condition': 'service_healthy'}},
                'healthcheck': {
                    'test': ['CMD', 'python3', '-c', "import urllib.request; urllib.request.urlopen('http://127.0.0.1:8080/api/health', timeout=2).close()"],
                    'interval': '10s', 'timeout': '5s', 'retries': 30, 'start_period': '20s',
                },
                'security_opt': ['no-new-privileges:true'],
                'restart': 'unless-stopped',
                'stop_grace_period': '90s',
                'logging': {'driver': 'json-file', 'options': {'max-size': '10m', 'max-file': '3'}},
            },
        },
    }
    output.mkdir(mode=0o700)
    try:
        files = {
            '.env': 'LOOPFORGE_DB_PASSWORD=' + password + '\nLOOPFORGE_DB_ROOT_PASSWORD=' + root_password + '\nAGENTFLOW_SESSION_SECRET=' + session + '\n',
            'compose.yaml': json.dumps(compose, indent=2) + '\n',
            'REQUIRES-VALIDATION.txt': (
                'Prepared configuration only; not a deployed or service-ready installation.\n'
                'Requires HTTPS reverse proxy, reviewed initial account provisioning and schema/catalog setup.\n'
                'No Docker socket or NAS share root is mounted. Execution requires external runners.\n'
                'This profile does not implement full snapshot handoff, A2A or Windows onboarding.\n'
                'Keep .env private; do not commit or paste it.\n'
            ),
        }
        for name, content in files.items():
            fd = os.open(str(output / name), os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
            with os.fdopen(fd, 'w') as stream:
                stream.write(content)
    except BaseException:
        shutil.rmtree(output)
        raise
    return output


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--output', required=True)
    parser.add_argument('--data-root', required=True)
    parser.add_argument('--image', required=True)
    parser.add_argument('--bind', default='127.0.0.1')
    parser.add_argument('--port', type=int, default=8090)
    parser.add_argument('--project', default='loopforge-nas')
    args = parser.parse_args()
    try:
        prepare(**vars(args))
    except (OSError, ValueError) as error:
        print('Cannot prepare bundle: ' + str(error), file=sys.stderr)
        return 1
    print('Prepared private bundle. No services were started. Review REQUIRES-VALIDATION.txt.')
    return 0


if __name__ == '__main__':
    sys.exit(main())
