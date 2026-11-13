#!/usr/bin/env python3
"""Check skill ownership across installs and uninstalls in private fixtures."""

import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[1]
RECEIPT = '.looper-installed-skills'
CORE = {'looper-diagnose', 'looper-backlog'}
WORKFLOW = {'git-commit', 'github-release'}


class SkillInstallTest(unittest.TestCase):
    def setUp(self):
        # Keep generated files in the ignored build directory.
        (ROOT / 'build').mkdir(exist_ok=True)
        prefix = os.environ.get('LOOPER_RESOURCE_PREFIX', 'looper') + '-skills-'
        temporary = tempfile.TemporaryDirectory(prefix=prefix, dir=ROOT / 'build')
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.package = self.root / 'package'
        self.package.mkdir()
        for script in ('install.sh', 'uninstall.sh'):
            shutil.copy2(ROOT / script, self.package / script)
        shutil.copytree(ROOT / 'skills', self.package / 'skills')
        self.skills = self.root / 'user skills'
        self.home = self.root / 'home'
        self.home.mkdir()
        self.user_prefix = self.root / 'user-prefix'
        self.args = ['--skip-bin', '--skills-dir', str(self.skills)]

    def installer_env(self):
        # T056: installer fixtures must never reach the operator's prefix.
        # HOME and LOOPER_USER_PREFIX point into this sandbox and inherited
        # PREFIX/CODEX_HOME values are dropped; an explicit --prefix on the
        # command line still wins inside the scripts.
        env = {k: v for k, v in os.environ.items()
               if k not in ('HOME', 'PREFIX', 'CODEX_HOME', 'LOOPER_USER_PREFIX')}
        env['HOME'] = str(self.home)
        env['LOOPER_USER_PREFIX'] = str(self.user_prefix)
        for value in (env['HOME'], env['LOOPER_USER_PREFIX']):
            assert Path(value).is_relative_to(self.root), value
        return env

    def run_script(self, script, *args, success=True):
        result = subprocess.run(
            ['bash', str(self.package / script), *self.args, *args],
            env=self.installer_env(), text=True, capture_output=True)
        self.assertEqual(result.returncode == 0, success, result.stdout + result.stderr)
        # Nothing may ever appear at the sandbox HOME's default locations.
        self.assertFalse((self.home / '.local').exists())
        self.assertFalse((self.home / '.codex').exists())
        return result

    def user_skill(self, name):
        path = self.skills / name / 'SKILL.md'
        path.parent.mkdir(parents=True)
        path.write_text('User-authored skill.\n')
        return path

    def installed(self):
        return set((self.skills / RECEIPT).read_text().splitlines())

    def test_default_uninstall_preserves_user_workflow_skills(self):
        self.run_script('install.sh')
        user = self.user_skill('git-commit')
        other = self.user_skill('github-release')
        self.assertEqual(self.installed(), CORE)
        self.run_script('uninstall.sh')
        self.assertEqual(user.read_text(), 'User-authored skill.\n')
        self.assertEqual(other.read_text(), 'User-authored skill.\n')
        self.assertEqual({p.name for p in self.skills.iterdir()}, WORKFLOW)
        self.run_script('uninstall.sh')  # Repeated removal preserves user files.
        self.assertTrue(user.exists())

    def test_existing_selected_skills_are_never_claimed(self):
        user = self.user_skill('looper-backlog')
        workflow = self.user_skill('git-commit')
        self.run_script('install.sh', '--with-workflow-skills')
        self.assertEqual(self.installed(), {'looper-diagnose', 'github-release'})
        self.assertEqual(user.read_text(), 'User-authored skill.\n')
        self.assertEqual(workflow.read_text(), 'User-authored skill.\n')
        self.run_script('uninstall.sh')
        self.assertTrue(user.exists())
        self.assertTrue(workflow.exists())

    def test_reinstall_retains_workflow_ownership_and_updates_owned_skills(self):
        self.run_script('install.sh', '--with-workflow-skills')
        owned = self.skills / 'looper-backlog' / 'SKILL.md'
        owned.write_text('Old install.\n')
        self.run_script('install.sh')
        self.assertEqual(owned.read_bytes(),
                         (self.package / 'skills/looper-backlog/SKILL.md').read_bytes())
        self.assertEqual(self.installed(), CORE | WORKFLOW)
        self.assertEqual(len((self.skills / RECEIPT).read_text().splitlines()), 4)
        # Removal must also work without the source skill tree.
        shutil.rmtree(self.package / 'skills')
        self.run_script('uninstall.sh')
        self.assertEqual(list(self.skills.iterdir()), [])

    def test_dry_run_and_skip_skills_preserve_receipt(self):
        self.run_script('install.sh', '--dry-run')
        self.assertFalse(self.skills.exists())
        self.run_script('install.sh')
        before = (self.skills / RECEIPT).read_bytes()
        self.run_script('install.sh', '--dry-run', '--with-workflow-skills')
        self.run_script('install.sh', '--skip-skills', '--with-workflow-skills')
        self.run_script('uninstall.sh', '--dry-run')
        self.run_script('uninstall.sh', '--skip-skills')
        self.assertEqual((self.skills / RECEIPT).read_bytes(), before)
        self.assertEqual({p.name for p in self.skills.iterdir()}, CORE | {RECEIPT})

    def test_no_receipt_preserves_legacy_skills(self):
        paths = [self.user_skill(name) for name in CORE | WORKFLOW]
        self.run_script('uninstall.sh')
        for path in paths:
            self.assertEqual(path.read_text(), 'User-authored skill.\n')

    def test_invalid_receipt_removes_nothing(self):
        self.run_script('install.sh')
        receipt = self.skills / RECEIPT
        for content in ('looper-backlog\n../package\n', 'looper-backlog'):
            with self.subTest(content=content):
                receipt.write_text(content)
                self.run_script('uninstall.sh', success=False)
                self.run_script('install.sh', success=False)
                self.assertTrue((self.skills / 'looper-backlog').exists())
                self.assertTrue(self.package.exists())

    def test_symlinks_are_not_overwritten_or_claimed(self):
        user = self.user_skill('custom')
        (self.skills / 'looper-backlog').symlink_to(user.parent, target_is_directory=True)
        self.run_script('install.sh')
        self.assertEqual(self.installed(), {'looper-diagnose'})
        self.run_script('uninstall.sh')
        self.assertEqual(user.read_text(), 'User-authored skill.\n')
        self.assertTrue((self.skills / 'looper-backlog').is_symlink())

    def test_receipt_symlink_is_rejected(self):
        user = self.user_skill('looper-backlog')
        receipt = self.root / 'unrelated receipt'
        receipt.write_text('looper-backlog\n')
        (self.skills / RECEIPT).symlink_to(receipt)
        self.run_script('install.sh', success=False)
        self.run_script('uninstall.sh', success=False)
        self.assertEqual(receipt.read_text(), 'looper-backlog\n')
        self.assertTrue(user.exists())

    def test_default_locations_stay_in_bound_prefix(self):
        # The incident shape: a no-options invocation must not install into
        # the real user prefix. With LOOPER_USER_PREFIX bound, bin and skills
        # land inside it, and the sandbox HOME stays empty.
        (self.package / 'build').mkdir()
        binary = self.package / 'build/looper'
        binary.write_text('#!/bin/sh\necho stub looper\n')
        binary.chmod(0o755)
        self.args = []
        self.run_script('install.sh')
        self.assertEqual((self.user_prefix / 'bin/looper').read_text(),
                         '#!/bin/sh\necho stub looper\n')
        skills = self.user_prefix / 'share/looper/skills'
        self.assertTrue((skills / 'looper-diagnose/SKILL.md').is_file())
        self.assertTrue((skills / 'looper-backlog/SKILL.md').is_file())
        self.assertTrue((skills / '.looper-installed-skills').is_file())
        self.assertEqual(list(self.home.iterdir()), [])
        self.run_script('uninstall.sh')
        self.assertFalse((self.user_prefix / 'bin/looper').exists())
        self.assertFalse((skills / 'looper-diagnose').exists())
        self.assertFalse((skills / '.looper-installed-skills').exists())

    def test_location_options_use_the_matching_receipt(self):
        for option in ('--prefix', '--codex-home'):
            with self.subTest(option=option):
                location = self.root / option[2:]
                self.args = ['--skip-bin', option, str(location)]
                self.skills = location / ('share/looper/skills' if option == '--prefix' else 'skills')
                self.run_script('install.sh')
                user = self.user_skill('git-commit')
                self.assertEqual(self.installed(), CORE)
                self.run_script('uninstall.sh')
                self.assertTrue(user.exists())
                self.assertFalse((self.skills / RECEIPT).exists())


if __name__ == '__main__':
    unittest.main()
