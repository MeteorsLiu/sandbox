import pathlib
import tempfile
import unittest
from unittest import mock

from memory import attribute, callback_memory, callback_windows, resident_memory, smaps


class MemoryTest(unittest.TestCase):
    def test_smaps_reads_residency_not_mapping_size(self):
        mappings = smaps("""1000-9000 rw-p 00000000 00:00 0 [anon: Sentry Go: heap]
Size:                 32 kB
Rss:                   8 kB
Pss:                   6 kB
9000-a000 rw-s 00000000 00:01 42 /memfd:llar-runtime-memory (deleted)
Pss:                   2 kB
""")
        self.assertEqual([m["pss"] for m in mappings], [6144, 2048])
        self.assertEqual(mappings[1]["file"], (0, 1, 42))

    def test_shared_memfd_counted_once_and_host_excluded(self):
        def mapping(name, pss, file=(0, 0, 0)):
            return {"name": name, "pss": pss, "file": file}
        shared = mapping("/memfd:llar-runtime-memory (deleted)", 10, (0, 1, 42))
        processes = {
            10: {"parent": 1, "mappings": [
                mapping("[anon: Go: heap]", 1000),
                mapping("/opt/benchmark/formula-bench", 200),
                mapping("[anon: Sentry Go: heap]", 30),
                mapping("/opt/benchmark/sentrylib.so", 20), shared,
                mapping("", 7)]},
            11: {"parent": 10, "mappings": [shared, shared, mapping("[stack]", 3)]},
        }
        result = attribute(processes, {(0, 1, 42): {"category": "guest_backing_bytes", "bytes": 80}})
        self.assertEqual(result["guest_backing_bytes"], 80)
        self.assertEqual(result["sentry_guest_bytes"], 133)
        self.assertEqual(result["host_runtime_pss_bytes"], 1000)
        self.assertEqual(result["host_image_pss_bytes"], 200)
        self.assertEqual(result["unattributed_pss_bytes"], 7)
        self.assertEqual(result["pss_bytes"], 1290)

    def test_disappearing_process_esrch(self):
        with tempfile.TemporaryDirectory() as directory:
            group = pathlib.Path(directory)
            (group / "memory.swap.current").write_text("0")
            (group / "cgroup.procs").write_text("99999999")
            read = pathlib.Path.read_text
            def retiring(path, *args, **kwargs):
                if str(path).startswith("/proc/99999999/"):
                    raise ProcessLookupError(3, "No such process")
                return read(path, *args, **kwargs)
            with mock.patch.object(pathlib.Path, "read_text", retiring):
                self.assertEqual(resident_memory(group)["pids"], [])

    def test_swap_is_not_reported_as_resident(self):
        with tempfile.TemporaryDirectory() as directory:
            group = pathlib.Path(directory)
            (group / "memory.swap.current").write_text("4096")
            with self.assertRaisesRegex(RuntimeError, "swap"):
                resident_memory(group)

    def test_parallel_transfers_excluded(self):
        events = [(0, "run_start", 0), (10, "run_start", 1),
                  (20, "callback_start", 0), (30, "callback_start", 1),
                  (80, "callback_end", 0), (90, "result", 0),
                  (100, "callback_end", 1), (110, "result", 1)]
        record = {"container": "one", "start_ns": 1000, "wall_ns": 120,
                  "events": [{"event": kind, "id": id, "observed_ns": ns} for ns, kind, id in events]}
        self.assertEqual(callback_windows(record), [(1030, 1080), (1090, 1100)])
        samples = []
        for start, end in [(1040, 1060), (1070, 1085), (1082, 1088), (1093, 1098)]:
            samples.append({"begin_ns": start, "end_ns": end, "engine_bytes": 50,
                            "containers": {"one": {"bytes": 200, "resident": {
                                "sentry_runtime_pss_bytes": 10, "sentry_guest_bytes": 100, "pids": [1]}}}})
        result = callback_memory({"records": [record], "backend": "sandbox"}, samples)
        self.assertEqual(result["samples"], 2)
        self.assertEqual(result["errors"], [])
        self.assertEqual(result["bytes"]["sentry_guest_bytes"]["p50"], 100)
        self.assertEqual(result["bytes"]["with_docker_services_cgroup_bytes"]["p50"], 250)

    def test_missing_samples_fail_measurement(self):
        result = callback_memory({"records": [], "backend": "sandbox"}, [])
        self.assertEqual(result["samples"], 0)
        self.assertTrue(result["errors"])


if __name__ == "__main__":
    unittest.main()
