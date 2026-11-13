#!/usr/bin/env python3
"""Check binary installation: live destination warnings, sandboxed prefixes.

The fixtures drive the real install.sh from a package with a prebuilt stub
binary, so nothing compiles and nothing reaches the operator's prefix.
"""

import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[1]
WARNING = 'is in use by running process'
KEEPS_BUILD = 'running controllers keep their original build until they restart'


class BinaryInstallTest(unittest.TestCase):
    def setUp(self):
        # Keep generated files in the ignored build directory.
        (ROOT / 'build').mkdir(exist_ok=True)
        prefix = os.environ.get('LOOPER_RESOURCE_PREFIX', 'looper') + '-install-'
        temporary = tempfile.TemporaryDirectory(prefix=prefix, dir=ROOT / 'build')
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.package = self.root / 'package'
        self.package.mkdir()
        shutil.copy2(ROOT / 'install.sh', self.package / 'install.sh')
        # A Homebrew-shaped package: a prebuilt binary beside install.sh and
        # no go.mod, so install.sh installs the stub without compiling.
        self.source = self.package / 'looper'
        self.source.write_text('#!/bin/sh\necho fixture looper\n')
        self.source.chmod(0o755)
        self.home = self.root / 'home'
        self.home.mkdir()
        self.user_prefix = self.root / 'user-prefix'
        # T056: installer fixtures must never reach the operator's prefix.
        # HOME and LOOPER_USER_PREFIX point into this sandbox and inherited
        # PREFIX/CODEX_HOME values are dropped.
        self.env = {k: v for k, v in os.environ.items()
                    if k not in ('HOME', 'PREFIX', 'CODEX_HOME', 'LOOPER_USER_PREFIX')}
        self.env['HOME'] = str(self.home)
        self.env['LOOPER_USER_PREFIX'] = str(self.user_prefix)
        for value in (self.env['HOME'], self.env['LOOPER_USER_PREFIX']):
            assert Path(value).is_relative_to(self.root), value

    def run_install(self, *args, success=True):
        result = subprocess.run(
            ['bash', str(self.package / 'install.sh'), '--skip-skills', *args],
            env=self.env, text=True, capture_output=True)
        self.assertEqual(result.returncode == 0, success, result.stdout + result.stderr)
        # Nothing may ever appear at the sandbox HOME's default locations.
        self.assertFalse((self.home / '.local').exists())
        self.assertFalse((self.home / '.codex').exists())
        return result

    def live_destination(self):
        """Run a real executable from the destination so /proc/<pid>/exe
        names the exact path an install is about to replace."""
        self.destination = self.user_prefix / 'bin' / 'looper'
        self.destination.parent.mkdir(parents=True)
        sleeper = shutil.which('sleep')
        if sleeper is None:
            self.skipTest('this platform has no sleep executable')
        shutil.copy2(sleeper, self.destination)
        self.destination.chmod(0o755)
        self.process = subprocess.Popen([str(self.destination), '120'])
        self.addCleanup(self.stop_live_destination)
        exe = os.readlink('/proc/%d/exe' % self.process.pid)
        if exe.split(' (deleted)')[0] != os.path.realpath(self.destination):
            self.stop_live_destination()
            self.skipTest('this platform does not expose /proc/<pid>/exe')

    def stop_live_destination(self):
        if self.process.poll() is None:
            self.process.terminate()
            self.process.wait()

    def test_replacing_a_live_destination_warns(self):
        self.live_destination()
        before = self.destination.stat().st_ino
        result = self.run_install()
        output = result.stdout + result.stderr
        self.assertIn(WARNING, output)
        self.assertIn(KEEPS_BUILD, output)
        self.assertIn(str(self.process.pid), output)
        # The install still replaces the file on disk, and the running
        # process keeps its original build: it is still alive.
        self.assertEqual(self.destination.read_text(), '#!/bin/sh\necho fixture looper\n')
        self.assertNotEqual(self.destination.stat().st_ino, before)
        self.assertIsNone(self.process.poll())

    def test_dry_run_warns_without_touching_the_live_destination(self):
        self.live_destination()
        before = self.destination.read_bytes()
        result = self.run_install('--dry-run')
        output = result.stdout + result.stderr
        self.assertIn(WARNING, output)
        self.assertIn(KEEPS_BUILD, output)
        # A dry run changes nothing, so the live binary is untouched.
        self.assertEqual(self.destination.read_bytes(), before)
        self.assertIsNone(self.process.poll())

    def test_unused_destination_installs_without_the_warning(self):
        self.live_destination()
        self.stop_live_destination()
        self.process.wait()
        result = self.run_install()
        output = result.stdout + result.stderr
        self.assertNotIn(WARNING, output)
        self.assertNotIn(KEEPS_BUILD, output)
        self.assertEqual(self.destination.read_text(), '#!/bin/sh\necho fixture looper\n')

    def test_missing_destination_installs_without_the_warning(self):
        result = self.run_install()
        output = result.stdout + result.stderr
        self.assertNotIn(WARNING, output)
        destination = self.user_prefix / 'bin' / 'looper'
        self.assertEqual(destination.read_text(), '#!/bin/sh\necho fixture looper\n')


if __name__ == '__main__':
    unittest.main()
