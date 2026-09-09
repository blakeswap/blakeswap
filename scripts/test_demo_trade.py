import copy
import importlib.util
import io
import pathlib
import tempfile
import unittest
from contextlib import redirect_stdout
from unittest.mock import patch

from demo_trade import whole_offer, whole_take, matching_parent

ROOT = pathlib.Path(__file__).resolve().parent


def load(name, file):
    spec = importlib.util.spec_from_file_location(name, ROOT / file)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


class DemoTradeTests(unittest.TestCase):
    def parent(self):
        return dict(whole_offer(), id="parent", maker="maker", network="regtest",
                    version=1, revision=1, status="open", available=1_000_000)

    def test_explicit_private_caps_and_exact_delivered_revision(self):
        plain = whole_offer()
        self.assertEqual(plain["fill_mode"], "whole")
        self.assertEqual((plain["min_fill"], plain["max_fill"]), (1_000_000, 1_000_000))
        self.assertEqual(plain["fee_budgets"], {"btc": 50000, "blake": 50000})
        self.assertEqual(plain["bounty_budgets"], {"btc": 0, "blake": 0})
        protected = whole_offer(tower_bps=50)
        self.assertEqual(protected["bounty_budgets"], {"btc": 5000, "blake": 10000})
        self.assertEqual(protected["funding_fee"], 2000)
        original = self.parent()
        observed = copy.deepcopy(original)
        observed["revision"] = str(2**64-1)
        observed["sell_amount"] = str(original["sell_amount"])
        result = whole_take(original, observed)
        self.assertEqual(result["quantity"], 1_000_000)
        self.assertEqual(result["parent_revision"], 2**64-1)
        self.assertEqual(result["funding_fee"], 2000)
        self.assertNotIn("fee_budgets", result)
        self.assertNotIn("bounty_budgets", result)

    def test_missing_foreign_changed_and_lossy_parent_refused(self):
        original = self.parent()
        for key, value in [("maker", "foreign"), ("id", "foreign"), ("network", "mainnet"),
                           ("version", 2), ("fill_mode", "partial"), ("available", 999999),
                           ("sell_amount", 1000001), ("buy_amount", 2000001), ("min_fill", 1),
                           ("revision", 0), ("revision", 2**64), ("revision", 1.5), ("revision", True)]:
            observed = dict(original, **{key: value})
            with self.subTest(key=key, value=value), self.assertRaises(ValueError):
                whole_take(original, observed)
        for key in ["revision", "version", "min_fill", "max_fill", "available"]:
            observed = dict(original); del observed[key]
            with self.subTest(missing=key), self.assertRaises((ValueError, KeyError)):
                whole_take(original, observed)
        with self.assertRaises(ValueError):
            whole_take(dict(original, version=2), original)
        self.assertFalse(matching_parent(original, dict(original, maker="foreign")))

    def test_both_real_demo_functions_forward_current_whole_snapshot(self):
        # Run the real orchestration with every RPC/process operation replaced;
        # no fixture daemon, node, credential file or external endpoint exists.
        for filename in ["desktop-demo.py", "dev.py"]:
            with self.subTest(script=filename), tempfile.TemporaryDirectory() as directory:
                module = load("demo_under_test", filename)
                original = self.parent(); current = dict(original, revision=str(2**64-1))
                creates, takes = [], []
                def call(profile, method="status", params=None):
                    if method == "offer.create":
                        creates.append(params); return original
                    if method == "swap.take":
                        takes.append(params); return {"id": "child"}
                    if method == "status":
                        return {"network": "regtest", "addresses": {"btc": "synthetic", "blake": "synthetic"},
                                "orders": [dict(current, maker="foreign"), current],
                                "swaps": [{"id": "child", "stage": "completed"}]}
                    if method in ["regtest.mine", "regtest.faucet"]:
                        return {}
                    self.fail("Unexpected operation " + method)
                with patch.object(module, "call", side_effect=call), redirect_stdout(io.StringIO()):
                    if filename == "desktop-demo.py":
                        with patch.object(module, "DATA", pathlib.Path(directory)), patch.object(module.local, "rpc", side_effect=AssertionError("No node read expected")):
                            module.trade()
                    else:
                        with patch.object(module, "LOCAL", pathlib.Path(directory)):
                            module.trade()
                self.assertEqual(len(creates), 1); self.assertEqual(len(takes), 1)
                self.assertEqual(creates[0], whole_offer(tower_bps=50 if filename == "dev.py" else 0))
                self.assertEqual(takes[0], whole_take(original, current))
