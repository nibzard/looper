#!/usr/bin/env python3
"""Exercise release installation with local archives and a fake curl."""
import io
import json
import os
from pathlib import Path
import shutil
import subprocess
import tarfile
import tempfile
import unittest

SCRIPT = Path(__file__).resolve().parent / 'install-latest.sh'
ROOT = Path(__file__).resolve().parents[1]


class InstallerTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.bin = self.root / 'bin'
        self.bin.mkdir()
        self.archive = self.root / 'fixture.tar.gz'
        self.make_archive()
        curl = self.bin / 'curl'
        curl.write_text('''#!/usr/bin/env python3
import json, os, pathlib, shutil, sys
args = sys.argv[1:]
with open(os.environ['CALLS'], 'a') as f: f.write(json.dumps(args) + '\\n')
if args[-1].endswith('/releases/latest'):
    print(os.environ.get('RELEASE_URL', 'https://github.com/nibzard/looper/releases/tag/v0.8.3'), end='')
else:
    if os.environ.get('FAIL_DOWNLOAD'): sys.exit(22)
    shutil.copyfile(os.environ['ARCHIVE'], args[args.index('--output') + 1])
''')
        curl.chmod(0o755)
        # T056: installer fixtures must never reach the operator's prefix.
        # HOME and LOOPER_USER_PREFIX point into this sandbox and inherited
        # PREFIX/CODEX_HOME values are dropped; the environment reaches the
        # inner install.sh unchanged.
        self.home = self.root / 'home'
        self.home.mkdir()
        self.user_prefix = self.root / 'user-prefix'
        self.env = {k: v for k, v in os.environ.items()
                    if k not in ('HOME', 'PREFIX', 'CODEX_HOME', 'LOOPER_USER_PREFIX')}
        self.env.update(PATH=f'{self.bin}:{os.environ["PATH"]}',
                        TMPDIR=str(self.root), CALLS=str(self.root / 'calls'),
                        ARCHIVE=str(self.archive), ARGS=str(self.root / 'args'),
                        HOME=str(self.home), LOOPER_USER_PREFIX=str(self.user_prefix))
        for value in (self.env['HOME'], self.env['LOOPER_USER_PREFIX']):
            assert Path(value).is_relative_to(self.root), value

    def make_archive(self, installer=True):
        with tarfile.open(self.archive, 'w:gz') as archive:
            if installer:
                data = b'#!/bin/bash\nprintf "%s\\n" "$@" > "$ARGS"\nexit "${INSTALL_EXIT:-0}"\n'
                info = tarfile.TarInfo('looper-0.8.3/install.sh')
                info.size = len(data)
                archive.addfile(info, io.BytesIO(data))

    def make_release_archive(self):
        """A release-shaped archive: the real install.sh, a stub binary, and skills."""
        stub = self.root / 'stub-looper'
        stub.write_text('#!/bin/sh\necho stub looper\n')
        stub.chmod(0o755)
        with tarfile.open(self.archive, 'w:gz') as archive:
            archive.add(ROOT / 'install.sh', arcname='looper-0.8.3/install.sh')
            archive.add(stub, arcname='looper-0.8.3/looper')
            for skill in ('looper-diagnose', 'looper-backlog'):
                archive.add(ROOT / 'skills' / skill,
                            arcname=f'looper-0.8.3/skills/{skill}')

    def run_installer(self, *args, success=True):
        result = subprocess.run(['bash', str(SCRIPT), *args], env=self.env,
                                capture_output=True, text=True)
        self.assertEqual(result.returncode == 0, success, result.stdout + result.stderr)
        self.assertFalse(list(self.root.glob('looper-install.*')), 'temporary files remain')
        # Nothing may ever appear at the sandbox HOME's default locations.
        self.assertFalse((self.home / '.local').exists())
        self.assertFalse((self.home / '.codex').exists())
        return result

    def test_latest_binary_only(self):
        self.run_installer('--bin-dir', str(self.root / 'with spaces'))
        self.assertEqual((self.root / 'args').read_text().splitlines(),
                         ['--bin-dir', str(self.root / 'with spaces'), '--skip-skills'])
        calls = [json.loads(line) for line in (self.root / 'calls').read_text().splitlines()]
        self.assertEqual(len(calls), 2)
        self.assertEqual(calls[-1][-1], 'https://github.com/nibzard/looper/archive/refs/tags/v0.8.3.tar.gz')

    def test_pinned_release_with_skills(self):
        self.run_installer('--version', 'v0.8.3', '--with-skills', '--prefix', '/some prefix')
        self.assertEqual((self.root / 'args').read_text().splitlines(), ['--prefix', '/some prefix'])
        self.assertEqual(len((self.root / 'calls').read_text().splitlines()), 1)

    def test_with_skills_without_value_options(self):
        # install_args stays empty; the guarded expansion passes no arguments.
        self.run_installer('--version', 'v0.8.3', '--with-skills')
        self.assertEqual((self.root / 'args').read_text().strip(), '')

    def test_real_installer_stays_in_bound_prefix(self):
        # The incident shape (T056): a bare install of a downloaded release
        # used to write ~/.local/bin/looper and replace the operator's
        # binary. Under the bound LOOPER_USER_PREFIX the whole install lands
        # inside the sandbox prefix.
        self.make_release_archive()
        self.run_installer('--version', 'v0.8.3', '--with-skills')
        self.assertEqual((self.user_prefix / 'bin/looper').read_text(),
                         '#!/bin/sh\necho stub looper\n')
        skills = self.user_prefix / 'share/looper/skills'
        self.assertTrue((skills / 'looper-diagnose/SKILL.md').is_file())
        self.assertTrue((skills / 'looper-backlog/SKILL.md').is_file())
        self.assertEqual(list(self.home.iterdir()), [])

    def test_real_installer_warns_for_live_destination(self):
        # AX-024: replacing a binary in live use warns, states that running
        # controllers keep their original build, and still installs.
        self.make_release_archive()
        destination = self.user_prefix / 'bin/looper'
        destination.parent.mkdir(parents=True)
        sleeper = shutil.which('sleep')
        if sleeper is None:
            self.skipTest('this platform has no sleep executable')
        shutil.copy2(sleeper, destination)
        destination.chmod(0o755)
        process = subprocess.Popen([str(destination), '120'])
        self.addCleanup(lambda: (process.terminate(), process.wait()))
        result = self.run_installer('--version', 'v0.8.3', '--with-skills')
        output = result.stdout + result.stderr
        self.assertIn('is in use by running process', output)
        self.assertIn('running controllers keep their original build', output)
        self.assertIn(str(process.pid), output)
        self.assertEqual(destination.read_text(), '#!/bin/sh\necho stub looper\n')
        self.assertIsNone(process.poll())

    def test_bad_options_and_tags(self):
        for args in [('--version',), ('--bin-dir',), ('--bad',), ('--version', '../main')]:
            with self.subTest(args=args):
                self.run_installer(*args, success=False)
        self.assertFalse((self.root / 'calls').exists())

    def test_bad_redirect(self):
        self.env['RELEASE_URL'] = 'https://example.com/v0.8.3'
        self.run_installer(success=False)
        self.assertFalse((self.root / 'args').exists())

    def test_failed_download(self):
        self.env['FAIL_DOWNLOAD'] = '1'
        self.run_installer(success=False)
        self.assertFalse((self.root / 'args').exists())

    def test_missing_installer(self):
        self.make_archive(installer=False)
        self.run_installer(success=False)

    def test_install_failure(self):
        self.env['INSTALL_EXIT'] = '7'
        result = self.run_installer(success=False)
        self.assertEqual(result.returncode, 7)

    def test_help(self):
        self.run_installer('--help')
        self.assertFalse((self.root / 'calls').exists())


if __name__ == '__main__':
    unittest.main()
