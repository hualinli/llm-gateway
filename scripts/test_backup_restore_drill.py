"""Test the embedded drill's asynchronous drain wait without starting the lab."""

import ast
from pathlib import Path
import types
import unittest


def load_waiter(clock):
    script = Path(__file__).with_name("backup-restore-drill.sh").read_text()
    driver = script.split("<<'DRIVER_EOF'\n", 1)[1].split("\nDRIVER_EOF", 1)[0]
    tree = ast.parse(driver)
    drill = next(node for node in tree.body if isinstance(node, ast.ClassDef) and node.name == "Drill")
    method = next(node for node in drill.body if isinstance(node, ast.FunctionDef) and node.name == "wait_backlog_drained")
    namespace = {"time": types.SimpleNamespace(monotonic=clock.monotonic, sleep=clock.sleep)}
    exec(compile(ast.Module(body=[method], type_ignores=[]), str(Path(__file__)), "exec"), namespace)
    return namespace[method.name]


class Clock:
    now = 0

    def monotonic(self):
        return self.now

    def sleep(self, seconds):
        self.now += seconds


class Metering:
    def __init__(self, snapshots):
        self.snapshots = list(snapshots)
        self.calls = 0
        self.timeouts = []

    def metering_json(self, path, timeout):
        assert path == "/receipts/backlog"
        self.calls += 1
        self.timeouts.append(timeout)
        return self.snapshots.pop(0) if len(self.snapshots) > 1 else self.snapshots[0]


def snapshot(pending=0, unsettled=0):
    backlog = dict.fromkeys(("pending_dispatch", "dispatched", "exception", "claimed", "unsettled"), 0)
    backlog["unsettled"] = unsettled
    return 200, {"backlog": backlog, "transport": {"pending": pending}}


class DrainWaitTests(unittest.TestCase):
    def setUp(self):
        self.clock = Clock()
        self.wait = load_waiter(self.clock)

    def test_waits_for_ack_after_database_settlement(self):
        env = Metering([snapshot(pending=1), snapshot()])
        drained, status, payload = self.wait(None, env, timeout=2)
        self.assertTrue(drained)
        self.assertEqual(env.calls, 2)
        self.assertEqual((status, payload), snapshot())

    def test_requires_database_and_transport_to_drain(self):
        env = Metering([snapshot(unsettled=1), snapshot(pending=1), snapshot()])
        self.assertTrue(self.wait(None, env, timeout=2)[0])
        self.assertEqual(env.calls, 3)

    def test_timeout_retains_last_snapshot(self):
        env = Metering([snapshot(pending=1)])
        drained, status, payload = self.wait(None, env, timeout=1)
        self.assertFalse(drained)
        self.assertEqual((status, payload), snapshot(pending=1))
        self.assertEqual(self.clock.now, 1)
        self.assertEqual(env.timeouts, [1, 0.5])

    def test_missing_state_and_unavailable_api_do_not_pass(self):
        for response in [(503, None), (200, {}), (200, {"backlog": {}, "transport": {"pending": 0}})]:
            with self.subTest(response=response):
                clock = Clock()
                self.assertFalse(load_waiter(clock)(None, Metering([response]), timeout=1)[0])


if __name__ == "__main__":
    unittest.main()
