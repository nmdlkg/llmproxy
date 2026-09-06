import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

import rollout as r


class RolloutTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        root = Path(self.temp.name)
        self.patches = []
        for name, value in {'ROOT': root / 'releases', 'STATE': root / 'state',
                            'STAGING': root / 'staging', 'CONFIG': root / 'config.yaml', 'BINARY': root / 'bin/proxy'}.items():
            p = patch.object(r, name, value)
            p.start()
            self.patches.append(p)
        for directory in [r.ROOT, r.STATE, r.STAGING, r.BINARY.parent]: directory.mkdir()
        self.old = r.ROOT / 'old'
        self.new = r.ROOT / 'new'
        for directory, data in [(self.old, 'old-binary'), (self.new, 'new-binary')]:
            directory.mkdir()
            (directory / 'plugins').mkdir()
            (directory / 'cliproxyapi').write_text(data)
            (directory / 'plugins/test.so').write_text(data + '-plugin')
            (directory / 'config.yaml').write_text('port: 8317\nplugins:\n  dir: ' + str(directory / 'plugins') + '\n')
        r.CONFIG.write_text((self.old / 'config.yaml').read_text())
        r.BINARY.write_text('old-binary')
        (r.STATE / 'current').write_text('old')
        (r.STATE / 'new.source-config').write_text(r.digest(r.CONFIG))
        (r.STATE / 'new.staged.json').write_text(json.dumps(r.manifest(self.new)))
        key = r.STATE / 'probe.key'
        key.write_text('test-key')
        (r.STATE / 'settings.json').write_text(json.dumps({'url': 'http://localhost:8317', 'key_file': str(key)}))
        self.calls = []
        for p in [patch.object(r.shutil, 'chown'), patch.object(r, 'run', side_effect=self.command),
                  patch.object(r, 'probe', return_value=True), patch.object(r, 'time')]:
            p.start()
            self.patches.append(p)
        r.time.time.return_value = 100

    def tearDown(self):
        for p in reversed(self.patches): p.stop()
        self.temp.cleanup()

    def command(self, *args):
        self.calls.append(args)
        if args[:2] == ('systemctl', 'show'):
            return 'ActiveState=active\nMainPID=123\nNRestarts=0'
        return ''

    def test_success_keeps_exact_staged_binary_and_records_previous(self):
        with patch.object(r, 'monitor'):
            r.promote('new')
        self.assertEqual(r.BINARY.read_text(), 'new-binary')
        self.assertEqual((r.STATE / 'current').read_text(), 'new')
        self.assertFalse((r.STATE / 'pending.json').exists())
        self.assertEqual(json.loads((r.STATE / 'last-result.json').read_text())['status'], 'accepted')
        self.assertTrue((r.ROOT / 'rollback-new/plugins/test.so').exists())

    def test_failed_monitor_restores_previous_artifacts_and_preserves_data(self):
        database = r.STATE.parent / 'production.db'
        database.write_text('new usage must survive')
        with patch.object(r, 'monitor', side_effect=RuntimeError('injected failure')):
            with self.assertRaises(RuntimeError): r.promote('new')
        self.assertEqual(r.BINARY.read_text(), 'old-binary')
        self.assertIn('rollback-new/plugins', r.CONFIG.read_text())
        self.assertEqual(database.read_text(), 'new usage must survive')
        self.assertEqual(json.loads((r.STATE / 'last-result.json').read_text())['status'], 'rolled-back')
        self.assertFalse((r.STATE / 'pending.json').exists())

    def test_start_failure_rolls_back(self):
        original = self.command
        starts = 0
        def fail_once(*args):
            nonlocal starts
            if args[:2] == ('systemctl', 'start'):
                starts += 1
                if starts == 1: raise RuntimeError('injected startup failure')
            return original(*args)
        with patch.object(r, 'run', side_effect=fail_once):
            with self.assertRaises(RuntimeError): r.promote('new')
        self.assertEqual(r.BINARY.read_text(), 'old-binary')
        self.assertEqual(starts, 2)

    def test_killed_worker_recovered_at_boot_without_starting_service(self):
        (r.STATE / 'pending.json').write_text(json.dumps({'candidate': 'new', 'previous': 'old'}))
        r.switch('new')
        r.recover(boot=True)
        self.assertEqual(r.BINARY.read_text(), 'old-binary')
        self.assertEqual(self.calls, [])
        self.assertFalse((r.STATE / 'pending.json').exists())

    def test_bad_recovery_remains_pending_for_operator(self):
        (r.STATE / 'pending.json').write_text(json.dumps({'candidate': 'new', 'previous': 'old'}))
        with patch.object(r, 'probe', return_value=False):
            with self.assertRaises(RuntimeError): r.recover()
        self.assertTrue((r.STATE / 'pending.json').exists())
        self.assertEqual(r.BINARY.read_text(), 'old-binary')

    def test_tampered_staged_artifact_cannot_stop_production(self):
        (self.new / 'cliproxyapi').write_text('tampered')
        with self.assertRaises(ValueError): r.promote('new')
        self.assertEqual(self.calls, [])
        self.assertEqual(r.BINARY.read_text(), 'old-binary')

    def test_changed_live_configuration_cannot_stop_production(self):
        r.CONFIG.write_text(r.CONFIG.read_text() + 'debug: true\n')
        with self.assertRaises(ValueError): r.promote('new')
        self.assertEqual(self.calls, [])

    def test_manual_rollback_after_acceptance(self):
        with patch.object(r, 'monitor'): r.promote('new')
        r.rollback()
        self.assertEqual(r.BINARY.read_text(), 'old-binary')
        with self.assertRaises(ValueError): r.rollback()

    def test_staging_failure_does_not_switch_production(self):
        bundle = r.STATE.parent / 'bundle'
        bundle.mkdir()
        (bundle / 'cliproxyapi').write_text('candidate')
        (bundle / 'manifest.json').write_text(json.dumps({'cliproxyapi': r.digest(bundle / 'cliproxyapi')}))
        with patch.object(r, 'run', side_effect=RuntimeError('staging fails')):
            with self.assertRaises(RuntimeError): r.stage(bundle)
        self.assertEqual(r.BINARY.read_text(), 'old-binary')
        self.assertFalse((r.STATE / 'bundle.staged.json').exists())

    def test_monitor_rejects_three_consecutive_failures(self):
        r.time.monotonic.side_effect = [0, 0, 1, 2]
        with patch.object(r, 'probe', return_value=False):
            with self.assertRaisesRegex(RuntimeError, 'three times'):
                r.monitor('new', {'url': 'http://localhost', 'key_file': 'unused'})

    def test_monitor_tolerates_one_transient_failure(self):
        r.time.monotonic.side_effect = [0, 0, 1, 2, 3, 181]
        with patch.object(r, 'probe', side_effect=[False, True, True, True]):
            r.monitor('new', {'url': 'http://localhost', 'key_file': 'unused'})

    def test_monitor_rejects_startup_crash_loop(self):
        with patch.object(r, 'properties', return_value={'NRestarts': '1'}):
            with self.assertRaisesRegex(RuntimeError, 'restarted'):
                r.monitor('new', {})


if __name__ == '__main__': unittest.main()
