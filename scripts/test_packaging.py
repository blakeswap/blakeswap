import importlib.util
import os
import pathlib
import plistlib
import subprocess
import tempfile
import unittest
from unittest.mock import patch

root = pathlib.Path(__file__).resolve().parents[1]
spec = importlib.util.spec_from_file_location('package_version', root / 'scripts/package-version.py')
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)


class PackagingTests(unittest.TestCase):
    def test_release_versions_and_reject_unsafe_values(self):
        for value in ['v1.2.3', '2.0.0-rc.1', 'v0.2.1']:
            with patch.dict(os.environ, {'BLAKESWAP_VERSION': value}):
                self.assertEqual(module.version(), value.removeprefix('v'))
        for value in ['', 'main', 'v1.2', 'v01.2.3', '1.2.3/evil', '1.2.3\nfoo', '$(id)']:
            with patch.dict(os.environ, {'BLAKESWAP_VERSION': value}):
                with self.assertRaises(ValueError): module.version()

    def test_plist_matches_tag_and_preserves_bundle_metadata(self):
        with tempfile.TemporaryDirectory() as directory:
            path = pathlib.Path(directory) / 'Info.plist'
            path.write_bytes(plistlib.dumps({'CFBundleIdentifier': 'org.blakeswap.app'}))
            env = dict(os.environ, BLAKESWAP_VERSION='v1.2.3-rc.1', BLAKESWAP_BUILD_NUMBER='42')
            subprocess.run(['python3', str(root / 'scripts/package-version.py'), str(path)], env=env, check=True)
            info = plistlib.loads(path.read_bytes())
            self.assertEqual(info['CFBundleIdentifier'], 'org.blakeswap.app')
            self.assertEqual(info['CFBundleShortVersionString'], '1.2.3')
            self.assertEqual(info['CFBundleVersion'], '42')
            self.assertEqual(info['BlakeswapReleaseVersion'], '1.2.3-rc.1')
