#!/usr/bin/env python3
"""Exercise release installation with local archives and a fake curl."""
import io
import json
import os
from pathlib import Path
import subprocess
import tarfile
import tempfile
import unittest

SCRIPT = Path(__file__).resolve().parent / 'install-latest.sh'


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
        self.env = dict(os.environ, PATH=f'{self.bin}:{os.environ["PATH"]}',
                        TMPDIR=str(self.root), CALLS=str(self.root / 'calls'),
                        ARCHIVE=str(self.archive), ARGS=str(self.root / 'args'))

    def make_archive(self, installer=True):
        with tarfile.open(self.archive, 'w:gz') as archive:
            if installer:
                data = b'#!/bin/bash\nprintf "%s\\n" "$@" > "$ARGS"\nexit "${INSTALL_EXIT:-0}"\n'
                info = tarfile.TarInfo('looper-0.8.3/install.sh')
                info.size = len(data)
                archive.addfile(info, io.BytesIO(data))

    def run_installer(self, *args, success=True):
        result = subprocess.run(['bash', str(SCRIPT), *args], env=self.env,
                                capture_output=True, text=True)
        self.assertEqual(result.returncode == 0, success, result.stdout + result.stderr)
        self.assertFalse(list(self.root.glob('looper-install.*')), 'temporary files remain')
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

    def test_bad_options_and_tags(self):
        for args in [('--version',), ('--bad',), ('--version', '../main')]:
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
