import contextlib
import io
import json
import pathlib
import tempfile
import unittest

from report import report


class ReportTest(unittest.TestCase):
    def write_batch(self, directory, measure, rate):
        directory.mkdir(parents=True, exist_ok=True)
        (directory / "environment.json").write_text(json.dumps({"host_process": {"cpus": [0, 1, 2, 3]}}))
        data = {"measure": measure, "backend": "docker", "concurrency": 1,
                "count": 1, "successful": 1, "builds_per_second": rate,
                "records": [{"success": True, "launch_to_main_ns": 1000000,
                             "launch_to_callback_ns": 2000000,
                             "events": [{"event": "result", "callback_duration_ns": 3000000}]}]}
        if measure == "memory":
            sizes = {"pss_bytes": 3, "docker_services_pss_bytes": 2,
                     "with_runtime_pss_bytes": 5, "with_docker_services_cgroup_bytes": 8}
            data["callback_memory"] = {"samples": 20, "errors": [], "bytes": {
                key: {"p50": size * 1048576, "p95": (size + 1) * 1048576}
                for key, size in sizes.items()}}
            data["engine_accounting"] = {"errors": []}
        (directory / "docker-c1.json").write_text(json.dumps(data))

    def test_timing_does_not_show_unsampled_memory(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = pathlib.Path(tmp)
            self.write_batch(root, "timing", 2)
            out = io.StringIO()
            with contextlib.redirect_stdout(out):
                report(root)
            self.assertIn("Timing Without Memory Sampling", out.getvalue())
            self.assertNotIn("Callback Memory", out.getvalue())
            self.assertNotIn("N/A", out.getvalue())

    def test_memory_does_not_report_profiled_throughput(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = pathlib.Path(tmp)
            self.write_batch(root, "memory", 999)
            out = io.StringIO()
            with contextlib.redirect_stdout(out):
                report(root)
            self.assertIn("Payload + Docker services PSS | 5.0 | 6.0", out.getvalue())
            self.assertNotIn("Builds/s", out.getvalue())
            self.assertNotIn("N/A", out.getvalue())

    def test_aggregate_excludes_preflight_and_profile_timings(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = pathlib.Path(tmp)
            self.write_batch(root / "timing/trial-1/docker", "timing", 2)
            self.write_batch(root / "timing/trial-2/docker", "timing", 4)
            self.write_batch(root / "memory/docker", "memory", 999)
            self.write_batch(root / "preflight", "timing", 777)
            out = io.StringIO()
            with contextlib.redirect_stdout(out):
                report(root)
            self.assertIn("docker | 1 | 2 | 2/2 | 3.0 | 2.0000 .. 4.0000", out.getvalue())
            self.assertIn("Payload + Docker services PSS | 5.0 | 6.0", out.getvalue())
            self.assertNotIn("999", out.getvalue())
            self.assertNotIn("777", out.getvalue())
            self.assertNotIn("N/A", out.getvalue())

    def test_missing_memory_is_an_error(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = pathlib.Path(tmp)
            self.write_batch(root, "timing", 2)
            path = root / "docker-c1.json"
            data = json.loads(path.read_text())
            data["measure"] = "memory"
            path.write_text(json.dumps(data))
            out = io.StringIO()
            with contextlib.redirect_stdout(out), self.assertRaises(SystemExit):
                report(root)
            self.assertIn("no samples", out.getvalue())

    def test_native_memory_has_no_cgroup_measurement(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = pathlib.Path(tmp)
            self.write_batch(root, "memory", 1)
            path = root / "docker-c1.json"
            data = json.loads(path.read_text())
            data["backend"] = "firecracker"
            del data["callback_memory"]["bytes"]["with_docker_services_cgroup_bytes"]
            path.write_text(json.dumps(data))
            out = io.StringIO()
            with contextlib.redirect_stdout(out):
                report(root)
            self.assertIn("Native VMM + guest PSS", out.getvalue())
            self.assertNotIn("N/A", out.getvalue())
            self.assertNotIn("services cgroup P50", out.getvalue())


if __name__ == "__main__":
    unittest.main()
