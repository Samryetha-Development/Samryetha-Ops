"""Readiness: API, actual executor probe, enabled identity and served release."""
import json
from pathlib import Path
from urllib.request import urlopen

def get(path):
    with urlopen('http://127.0.0.1:8791' + path, timeout=20) as response:
        return json.load(response)

assert get('/api/health')['ok'] is True
assert get('/api/judge')['ready'] is True, 'Docker judge is unavailable'
assert get('/api/auth/session')['enabled'] is True, 'Authentication is disabled'
assert Path('/opt/Nedmori/current/dist/index.html').is_file()
print('Nedmori API, Docker judge, OIDC configuration and frontend ready')
