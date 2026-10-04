import hashlib
import importlib.util
from pathlib import Path
import tempfile
import unittest


spec = importlib.util.spec_from_file_location(
    "parallel_soak", Path(__file__).with_name("soak-parallel-runs.py"))
soak = importlib.util.module_from_spec(spec)
spec.loader.exec_module(soak)


class ConfigurationIsolationTest(unittest.TestCase):
    def test_startup_state_is_adjacent_to_the_isolated_config(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            owner = root / "owner"
            runtime = root / "runtime"
            owner.mkdir()
            runtime.mkdir()
            config = owner / "config.yaml"
            state = owner / "model-state.json"
            config.write_text("models:\n  primary:\n    model: owner-model\n")
            state.write_text('{"running":"owner-model"}')
            before = [hashlib.sha256(p.read_bytes()).digest()
                      for p in (config, state)]
            isolated = soak.isolate_configuration(config, runtime)
            self.assertEqual(isolated.read_bytes(), config.read_bytes())
            self.assertEqual(isolated.stat().st_mode & 0o777, 0o600)
            # Model-change persistence derives its state path from config,
            # independently of SELF_STORAGE_DATA_DIR.
            isolated.with_name("model-state.json").write_text(
                '{"running":"temporary-provider"}')
            self.assertEqual(before, [hashlib.sha256(p.read_bytes()).digest()
                                     for p in (config, state)])


if __name__ == "__main__":
    unittest.main()
