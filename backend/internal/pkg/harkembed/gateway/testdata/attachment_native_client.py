"""Opt-in native Codex driver. Only a synthetic selected file and loopback gateway."""
import hashlib
import json
import os
from pathlib import Path
import subprocess
import sys
import tomllib
from urllib.parse import urlparse

endpoint, fixture, evidence = sys.argv[1:]
assert urlparse(endpoint).hostname == '127.0.0.1'
fixture, evidence = Path(fixture).resolve(), Path(evidence).resolve()
assert fixture.is_relative_to(evidence.parent), 'fixture must be in the external synthetic test workspace'
binary = Path(os.environ['CODEX_BINARY'])
real_config = Path.home() / '.codex' / 'config.toml'
saved = tomllib.loads(real_config.read_text(encoding='utf-8'))
assert saved['sandbox_mode'] == 'danger-full-access'
assert saved['windows']['sandbox'] == 'unelevated' and 'approval_policy' not in saved

def sha(p):
    return hashlib.sha256(p.read_bytes()).hexdigest()

before = {'binary': sha(binary), 'config': sha(real_config), 'fixture': sha(fixture)}
home = evidence / 'isolated-codex-home'
home.mkdir()
model = 'claude-opus-5-5'
config = f'''model = "{model}"
model_provider = "attachment_test"
sandbox_mode = "{saved['sandbox_mode']}"
[windows]
sandbox = "{saved['windows']['sandbox']}"
[model_providers.attachment_test]
name = "Actual local attachment gateway acceptance"
base_url = "{endpoint}/v1"
requires_openai_auth = false
supports_websockets = false
wire_api = "responses"
request_max_retries = 0
stream_max_retries = 0
'''
(home / 'config.toml').write_text(config, encoding='utf-8')
profile_hash = sha(home / 'config.toml')
allowed = {'SYSTEMROOT', 'WINDIR', 'PATH', 'PATHEXT', 'TEMP', 'TMP', 'USERPROFILE', 'LOCALAPPDATA', 'APPDATA',
           'PROGRAMFILES', 'PROGRAMDATA', 'COMSPEC', 'LANG', 'SYSTEMDRIVE', 'PUBLIC', 'USERNAME'}
env = {k: v for k, v in os.environ.items() if k.upper() in allowed}
env.update(CODEX_HOME=str(home), OTEL_SDK_DISABLED='true', CODEX_DISABLE_TELEMETRY='true', NO_PROXY='127.0.0.1,localhost')
prompt = ('# Files mentioned by the user:\n\n## ' + fixture.name + ': ' + fixture.as_posix() + '\n\n## My request:\n'
          'Read the hidden marker in the selected synthetic attachment and return it. Only this selected file may be read. '
          'The local gateway may request a raw PUT of this same file to its one-shot 127.0.0.1 upload endpoint. '
          'No other file or network is authorized. Preserve normal sandbox and approvals; stop on denial, never elevate or autoapprove.')
(evidence / 'PROMPT.txt').write_text(prompt, encoding='utf-8')
command = [str(binary), 'exec', '--ephemeral', '--skip-git-repo-check', '--json', '-C', str(fixture.parent),
           '-o', str(evidence / 'final.txt'), prompt]
proc = subprocess.run(command, cwd=fixture.parent, env=env, input=b'', capture_output=True, timeout=90,
                      creationflags=getattr(subprocess, 'CREATE_NO_WINDOW', 0))
(evidence / 'cli.stdout.txt').write_bytes(proc.stdout)
(evidence / 'cli.stderr.txt').write_bytes(proc.stderr)
after = {'binary': sha(binary), 'config': sha(real_config), 'fixture': sha(fixture)}
result = {'command': command, 'exit': proc.returncode, 'before': before, 'after': after,
          'real_files_unchanged': before == after, 'isolated_profile_unchanged': profile_hash == sha(home / 'config.toml'),
          'permission_overrides': False, 'approval_policy': 'unset, same as saved config'}
(evidence / 'CLIENT_RESULT.json').write_text(json.dumps(result, indent=2), encoding='utf-8')
print(json.dumps(result))
assert proc.returncode == 0 and before == after and result['isolated_profile_unchanged']
