import importlib.util
import json
import os
import pathlib
import subprocess
import tempfile
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location('local_nodes', pathlib.Path(__file__).with_name('local.py'))
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)


class LocalNodeTests(unittest.TestCase):
    def test_register_preserves_other_chain_and_refreshes_paths(self):
        with tempfile.TemporaryDirectory() as directory:
            registry = pathlib.Path(directory) / 'cache' / 'nodes.json'
            with patch.dict(os.environ, {'BLAKESWAP_REGTEST_REGISTRY': str(registry)}):
                with patch.object(module, 'ROOT', pathlib.Path(directory) / 'first checkout'):
                    module.register('btc'); module.register('blake')
                with patch.object(module, 'ROOT', pathlib.Path(directory) / 'second checkout'):
                    module.register('btc')
            nodes = json.loads(registry.read_text())
            self.assertIn('second checkout', nodes['btc']['cookie'])
            self.assertIn('first checkout', nodes['blake']['cookie'])
            self.assertEqual(nodes['btc']['url'], f"http://127.0.0.1:{module.NODES['btc'][1]}")
            self.assertEqual(registry.stat().st_mode & 0o777, 0o600)

    def test_make_targets_select_chain_and_register_only_on_request(self):
        root = pathlib.Path(__file__).resolve().parents[1]
        for target, chain in [('regtest-btc', 'btc'), ('regtest-blake', 'blake'), ('regtest-nodes', '')]:
            result = subprocess.run(['make', '-n', target], cwd=root, capture_output=True, text=True, check=True)
            self.assertIn(f"scripts/local.py nodes {chain + ' ' if chain else ''}--register", result.stdout)
            self.assertIn('scripts/bootstrap.py' + (' ' + chain if chain else ''), result.stdout)
