#!/usr/bin/env python3
"""Check skill helpers with local substitutes for external tools."""

import hashlib
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile

ROOT = Path(__file__).resolve().parents[1]
RELEASE = ROOT / 'skills/release-runbook/scripts/release.sh'
LIVE_TEST = ROOT / '.codex/skills/looper-live-test/scripts/run-live-test.sh'
TOOL = r'''#!/usr/bin/env python3
import hashlib
import io
import json
import os
from pathlib import Path
import sys
import tarfile

name = Path(sys.argv[0]).name
args = sys.argv[1:]
state_file = Path(os.environ['SKILL_TEST_STATE'])
state = json.loads(state_file.read_text())
state['calls'].append([name, *args])
code = 0

def changed():
    return [p for p, text in state['baseline'].items()
            if Path(p).read_text() != text]

if name == 'git':
    if args[0] == 'rev-parse':
        if '--verify' in args:
            code = 0 if os.environ.get('EXISTING_TAG') else 1
        elif '--show-toplevel' in args:
            print(os.getcwd())
        elif '--abbrev-ref' in args:
            print('main')
        else:
            print('true')
    elif args[0] == 'remote':
        print('https://github.com/example/project.git')
    elif args[0] == 'status':
        for p in changed():
            print(' M ' + p)
    elif args[0] == 'diff':
        if '--cached' in args:
            code = 1 if state['index'] else 0
        elif '--' in args:
            paths = args[args.index('--') + 1:]
            code = 1 if any(p in changed() for p in paths) else 0
        else:
            code = 1 if changed() else 0
    elif args[0] == 'add':
        paths = changed() if '-A' in args else args[args.index('--') + 1:]
        for p in paths:
            if p in changed():
                state['index'][p] = Path(p).read_text()
    elif args[0] == 'commit':
        state['commits'].append(sorted(state['index']))
        state['baseline'].update(state['index'])
        state['index'] = {}
    elif args[0] == 'tag':
        state['tags'].append(args[2])
    elif args[0] == 'push' and os.environ.get('FAIL_PUSH'):
        code = 7
elif name == 'gh':
    if args[:2] == ['repo', 'view']:
        print('example/project')
    elif args[:2] == ['release', 'create']:
        notes = Path(args[args.index('--notes-file') + 1]).read_text()
        state['notes'] = notes
        state['released'] = True
    elif args[:2] == ['release', 'view'] and os.environ.get('FAIL_RELEASE_VIEW'):
        code = 9
elif name == 'curl':
    assert '--fail' in args
    if os.environ.get('ARCHIVE_MODE') == 'failure':
        code = 22
    else:
        target = Path(args[args.index('--output') + 1])
        if os.environ.get('ARCHIVE_MODE') == 'invalid':
            target.write_text('<html>not an archive</html>')
        else:
            content = b'verified release archive\n'
            with tarfile.open(target, 'w:gz') as archive:
                item = tarfile.TarInfo('project/README.md')
                item.size = len(content)
                archive.addfile(item, io.BytesIO(content))
        state['archive_sha'] = hashlib.sha256(target.read_bytes()).hexdigest()
else:
    raise AssertionError(name)

state_file.write_text(json.dumps(state))
sys.exit(code)
'''


class Fixture:
    def __init__(self, folder, *, gh=True):
        self.root = Path(folder)
        self.repo = self.root / 'project'
        self.repo.mkdir()
        self.bin = self.root / 'bin'
        self.bin.mkdir()
        self.state_file = self.root / 'state.json'
        self.formula = self.repo / 'Formula/example.rb'
        self.formula.parent.mkdir()
        self.formula.write_text('class Example < Formula\n  url "old"\n  sha256 "old"\nend\n')
        (self.repo / 'VERSION').write_text('0.1.0\n')
        (self.repo / 'unrelated.txt').write_text('original\n')
        self.baseline = {p: (self.repo / p).read_text()
                         for p in ('VERSION', 'Formula/example.rb', 'unrelated.txt')}
        self.state_file.write_text(json.dumps({
            'baseline': self.baseline, 'index': {}, 'commits': [], 'calls': [],
            'tags': [], 'released': False,
        }))
        for name in ('git', 'curl', *(['gh'] if gh else [])):
            path = self.bin / name
            path.write_text(TOOL)
            path.chmod(0o755)
        for name in ('python3', 'bash', 'awk', 'mktemp', 'rm', 'mv', 'tar', 'gzip',
                     'cat', 'tee', 'mkdir', 'dirname', 'grep', 'jq', 'cp'):
            target = shutil.which(name)
            assert target, name
            (self.bin / name).symlink_to(target)
        for name in ('sha256sum', 'shasum', 'openssl'):
            target = shutil.which(name)
            if target:
                (self.bin / name).symlink_to(target)
        self.env = dict(os.environ, PATH=str(self.bin), SKILL_TEST_STATE=str(self.state_file))

    def run(self, *args, expected=0, env=None):
        result = subprocess.run(['/bin/bash', str(RELEASE), '--version', '0.2.0', *args],
                                cwd=self.repo, env=dict(self.env, **(env or {})),
                                capture_output=True, text=True, timeout=10)
        assert result.returncode == expected, result.stdout + result.stderr
        return result

    def state(self):
        return json.loads(self.state_file.read_text())


def check_release_cases():
    with tempfile.TemporaryDirectory() as temp:
        f = Fixture(temp)
        result = f.run('--dry-run', '--version-file', 'VERSION', '--test-cmd', 'check-final-version')
        assert (f.repo / 'VERSION').read_text() == '0.1.0\n'
        assert f.formula.read_text() == f.baseline['Formula/example.rb']
        assert not any(c[0] in ('gh', 'curl') for c in f.state()['calls'])
        assert not f.state()['tags'] and not f.state()['commits']
        assert result.stdout.index('set VERSION') < result.stdout.index('check-final-version')
        assert 'No release was published' in result.stdout

    with tempfile.TemporaryDirectory() as temp:
        f = Fixture(temp)
        notes = f.root / 'release notes.md'
        notes.write_text('Fix parsing.\n\nKeep `literal text` and $variables.\n')
        formula_mode = f.formula.stat().st_mode
        f.run('--version-file', 'VERSION', '--test-cmd', 'test "$(cat VERSION)" = 0.2.0',
              '--notes-file', str(notes))
        state = f.state()
        assert state['commits'] == [['VERSION'], ['Formula/example.rb']]
        assert state['tags'] == ['v0.2.0'] and state['released']
        assert state['notes'] == notes.read_text()
        assert state['archive_sha'] in f.formula.read_text()
        assert f.formula.stat().st_mode == formula_mode
        calls = state['calls']
        assert calls.index(['gh', 'release', 'view', 'v0.2.0']) < next(
            i for i, c in enumerate(calls) if c[0] == 'curl')

    for mode in ('failure', 'invalid'):
        with tempfile.TemporaryDirectory() as temp:
            f = Fixture(temp)
            result = f.run('--skip-tests', expected=1, env={'ARCHIVE_MODE': mode})
            assert f.formula.read_text() == f.baseline['Formula/example.rb']
            assert not f.state()['commits']
            assert f.state()['released']  # A partial release requires recovery.
            assert 'formula update' in result.stderr

    with tempfile.TemporaryDirectory() as temp:
        f = Fixture(temp)
        f.run('--version-file', 'VERSION', '--test-cmd', 'exit 8', expected=8)
        assert (f.repo / 'VERSION').read_text() == '0.2.0\n'
        assert not f.state()['tags'] and not f.state()['released']
        assert not f.state()['commits']

    with tempfile.TemporaryDirectory() as temp:
        f = Fixture(temp)
        f.run('--version-file', 'VERSION', expected=1)
        assert (f.repo / 'VERSION').read_text() == '0.1.0\n'
        assert not f.state()['calls']

    with tempfile.TemporaryDirectory() as temp:
        f = Fixture(temp, gh=False)
        f.run('--skip-tests', '--version-file', 'VERSION', expected=1)
        assert not f.state()['tags']
        assert (f.repo / 'VERSION').read_text() == '0.1.0\n'

    with tempfile.TemporaryDirectory() as temp:
        f = Fixture(temp)
        (f.repo / 'unrelated.txt').write_text('existing user edit\n')
        f.run('--allow-dirty', '--skip-tests', '--skip-formula', expected=1)
        f.run('--allow-dirty', '--stage-path', 'VERSION', '--version-file', 'VERSION',
              '--test-cmd', 'test "$(cat VERSION)" = 0.2.0', '--skip-formula')
        assert f.state()['commits'] == [['VERSION']]
        assert f.state()['baseline']['unrelated.txt'] == 'original\n'
        assert (f.repo / 'unrelated.txt').read_text() == 'existing user edit\n'
        assert not any(c == ['git', 'add', '-A'] for c in f.state()['calls'])

    with tempfile.TemporaryDirectory() as temp:
        f = Fixture(temp)
        state = f.state()
        state['index']['unrelated.txt'] = 'pre-existing staged edit\n'
        f.state_file.write_text(json.dumps(state))
        f.run('--allow-dirty', '--stage-path', 'VERSION', '--skip-tests', expected=1)
        assert not f.state()['tags'] and not f.state()['commits']

    with tempfile.TemporaryDirectory() as temp:
        f = Fixture(temp)
        f.run('--skip-tests', expected=1, env={'EXISTING_TAG': '1'})
        assert not f.state()['released']

    for flag in ('FAIL_PUSH', 'FAIL_RELEASE_VIEW'):
        with tempfile.TemporaryDirectory() as temp:
            f = Fixture(temp)
            f.run('--skip-tests', expected=7 if flag == 'FAIL_PUSH' else 1, env={flag: '1'})
            assert not any(c[0] == 'curl' for c in f.state()['calls'])

    with tempfile.TemporaryDirectory() as temp:
        f = Fixture(temp)
        result = f.run('--stage-path', 'unrelated.txt', '--version-file', 'VERSION',
                       '--skip-tests', expected=1)
        assert 'version files remain uncommitted' in result.stderr
        assert not f.state()['tags']

    with tempfile.TemporaryDirectory() as temp:
        f = Fixture(temp)
        f.run('--repo', 'not-a-repository', '--skip-tests', expected=1)
        assert not f.state()['tags'] and not f.state()['released']

    with tempfile.TemporaryDirectory() as temp:
        f = Fixture(temp)
        f.formula.write_text(f.formula.read_text() + '  url "second archive"\n')
        state = f.state()
        state['baseline']['Formula/example.rb'] = f.formula.read_text()
        f.state_file.write_text(json.dumps(state))
        result = f.run('--skip-tests', expected=1)
        assert 'one url and one sha256' in result.stderr
        assert not f.state()['tags']


def check_live_runner():
    stub = '''#!/usr/bin/env python3
import json
import os
from pathlib import Path
assert os.environ['LOOP_DELAY_SECONDS'] == '0'
assert os.environ['MAX_ITERATIONS'] == '1'
todo = Path('to-do.json')
data = json.loads(todo.read_text())
if not os.environ.get('BLOCKED_FIXTURE'):
    next(t for t in data['tasks'] if t['id'] == 'T2')['status'] = 'done'
    Path('README.md').write_text('Test output\\n')
    todo.write_text(json.dumps(data))
print('10:13:05  [1/1] T2  codex')
print('  Done  0s | 1 file | 1/2 done')
raise SystemExit(2)
'''
    with tempfile.TemporaryDirectory() as temp:
        f = Fixture(temp)
        fake = f.bin / 'looper-stub'
        fake.write_text(stub)
        fake.chmod(0o755)
        tmpdir = f.root / 'live'
        tmpdir.mkdir()
        env = dict(f.env, LOOPER_BIN=str(fake), TMPDIR=str(tmpdir))
        result = subprocess.run(['/bin/bash', str(LIVE_TEST)], cwd=f.repo,
                                env=env, capture_output=True, text=True, timeout=10)
        assert result.returncode == 0, result.stdout + result.stderr
        assert 'outcome verified' in result.stdout
        result = subprocess.run(['/bin/bash', str(LIVE_TEST)], cwd=f.repo,
                                env=dict(env, BLOCKED_FIXTURE='1'),
                                capture_output=True, text=True, timeout=10)
        assert result.returncode == 1, result.stdout + result.stderr
        assert 'expected T2 done' in result.stderr


check_release_cases()
check_live_runner()
print('Skill helper checks passed.')
