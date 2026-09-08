import json
import re
import unittest

import test_go_shard as shard


class GoShardTests(unittest.TestCase):
    def test_partition_covers_every_package_test_example_and_seed_once(self):
        tests = {(package, name) for package in ["a", "b"] for name in ["TestSame", "Example", "Example_wallet", "FuzzDecode", "TestUnicodeÅ", *[f"TestCase{i}" for i in range(80)]]}
        listed = shard.listed_tests("\n".join(json.dumps({"Action": "output", "Package": p, "Output": n + "\n"}) for p, n in tests))
        self.assertEqual(listed, tests)
        partitions = []
        for index in range(4):
            selected, pattern = shard.selection(listed, index, 4)
            self.assertEqual(selected, {test for test in tests if re.fullmatch(pattern, test[1])})
            self.assertFalse(any(selected & other for other in partitions))
            partitions.append(selected)
        self.assertEqual(set.union(*partitions), tests)

    def test_success_requires_each_discovered_test_to_start_and_finish_once(self):
        selected = {("a", "TestOne"), ("b", "FuzzSeed")}
        events = [{"Package": p, "Test": n, "Action": action} for p, n in selected for action in ["run", "pass"]]
        shard.verify_execution(selected, events)
        for bad in [events[:-1], events + [events[0]], events + [{"Package": "a", "Test": "TestUnexpected", "Action": "run"}]]:
            with self.assertRaises(ValueError):
                shard.verify_execution(selected, bad)
        with self.assertRaises(ValueError):
            shard.listed_tests("")
        with self.assertRaises(ValueError):
            shard.selection(selected, 4, 4)


if __name__ == "__main__":
    unittest.main()
