import fcntl
import importlib.util
import pathlib
import subprocess
import tempfile
import unittest

spec = importlib.util.spec_from_file_location('reset_local_data', pathlib.Path(__file__).with_name('reset-local-data.py'))
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)


class ResetLocalDataTests(unittest.TestCase):
    def test_make_target_handles_spaces_and_starts_with_no_active_storage(self):
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory) / 'Application Support' / 'Blakeswap'
            root.mkdir(parents=True)
            (root / 'settings.json').write_text('{}')
            result = subprocess.run(['make', 'reset-local-data', 'APP_DATA_DIR=' + str(root)],
                                    cwd=pathlib.Path(__file__).resolve().parents[1],
                                    capture_output=True, text=True, check=True)
            self.assertIn('Onboarding will appear', result.stdout)
            self.assertFalse(root.exists())
            self.assertEqual(len(list(root.parent.glob('Blakeswap-backup-*/settings.json'))), 1)

    def test_reset_archives_wallet_and_settings_without_deleting_them(self):
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory) / 'Blakeswap'
            root.mkdir()
            (root / 'settings.json').write_text('{}')
            (root / 'master.db').write_bytes(b'encrypted fixture')
            archive = module.reset(str(root))
            self.assertFalse(root.exists())
            self.assertEqual((archive / 'master.db').read_bytes(), b'encrypted fixture')
            self.assertEqual((archive / 'settings.json').read_text(), '{}')
            self.assertIsNone(module.reset(str(root)))

    def test_running_app_and_unrelated_paths_are_rejected(self):
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory) / 'Blakeswap'
            root.mkdir()
            with self.assertRaises(ValueError): module.reset(str(root))
            (root / 'settings.json').write_text('{}')
            with (root / 'desktop.lock').open('w') as lock:
                fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
                with self.assertRaisesRegex(ValueError, 'Quit Blakeswap'): module.reset(str(root))
                self.assertTrue(root.exists())
            link = pathlib.Path(directory) / 'link'
            link.symlink_to(root, target_is_directory=True)
            with self.assertRaises(ValueError): module.reset(str(link))
            self.assertTrue(root.exists())
        for bad in ['/', str(pathlib.Path.home()), 'relative']:
            with self.assertRaises(ValueError): module.reset(bad)
