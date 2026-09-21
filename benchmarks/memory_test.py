import errno
import pathlib
import subprocess
import sys
import tempfile
import unittest
from unittest import mock

from memory import attribute, callback_memory, callback_windows, process_tree, resident_memory, resident_processes, same_vm, smaps


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
            10: {"parent": 1, "vm": 10, "mappings": [
                mapping("[anon: Go: heap]", 1000),
                mapping("/opt/benchmark/formula-bench", 200),
                mapping("[anon: Sentry Go: heap]", 30),
                mapping("/opt/benchmark/sentrylib.so", 20), shared,
                mapping("", 7)]},
            11: {"parent": 10, "vm": 11, "mappings": [shared, shared, mapping("[stack]", 3)]},
        }
        result = attribute(processes, {(0, 1, 42): {"category": "guest_backing_bytes", "bytes": 80}})
        self.assertEqual(result["guest_backing_bytes"], 80)
        self.assertEqual(result["sentry_guest_bytes"], 133)
        self.assertEqual(result["host_runtime_pss_bytes"], 1000)
        self.assertEqual(result["host_image_pss_bytes"], 200)
        self.assertEqual(result["unattributed_pss_bytes"], 7)
        self.assertEqual(result["pss_bytes"], 1290)

    def test_shared_vm_counted_once_but_independent_identical_maps_preserved(self):
        mapping = {"name": "[stack]", "pss": 30, "file": (0, 0, 0)}
        processes = {
            10: {"parent": 1, "vm": 10, "mappings": []},
            11: {"parent": 10, "vm": 11, "mappings": [mapping]},
            12: {"parent": 11, "vm": 11, "mappings": [mapping]},
            13: {"parent": 12, "vm": 13, "mappings": [mapping]},
        }
        result = attribute(processes, {})
        self.assertEqual(result["pss_bytes"], 60)
        self.assertEqual(result["helper_pss_bytes"], 60)
        self.assertEqual(result["mm_groups"], [[10], [11, 12], [13]])

    def test_kcmp_returns_and_errors(self):
        for number in (312, 272):
            for result in (0, 1, 2, 3):
                with self.subTest(number=number, result=result), mock.patch("memory.SYS_KCMP", number), mock.patch("memory.syscall", return_value=result) as call:
                    self.assertEqual(same_vm(10, 11), result == 0)
                    self.assertEqual([value.value for value in call.call_args.args], [number, 10, 11, 1, 0, 0])
        for error in (errno.EPERM, errno.ENOSYS, errno.ESRCH):
            with self.subTest(error=error), mock.patch("memory.SYS_KCMP", 312), mock.patch("memory.syscall", return_value=-1), mock.patch("memory.ctypes.get_errno", return_value=error):
                with self.assertRaises(OSError) as raised:
                    same_vm(10, 11)
                self.assertEqual(raised.exception.errno, error)

    @unittest.skipUnless(sys.platform == "linux", "real CLONE_VM regression requires Linux")
    def test_linux_clone_vm_and_fork(self):
        with tempfile.TemporaryDirectory() as directory:
            binary = str(pathlib.Path(directory) / "shared-vm")
            subprocess.run(["cc", "-Wall", "-Wextra", "-Werror", "-O2", "-o", binary,
                            str(pathlib.Path(__file__).parent / "testdata/shared_vm.c")], check=True)
            proc = subprocess.Popen([binary], stdin=subprocess.PIPE, stdout=subprocess.PIPE, text=True)
            try:
                parent, shared, forked = map(int, proc.stdout.readline().split())
                self.assertTrue(same_vm(parent, shared))
                self.assertFalse(same_vm(parent, forked))
                measured = resident_processes([parent, shared, forked])
                self.assertEqual(measured["mm_groups"], [[parent, shared], [forked]])
                duplicated = sum(m["pss"] for pid in (parent, shared, forked)
                                 for m in smaps(pathlib.Path(f"/proc/{pid}/smaps").read_text()))
                self.assertGreater(duplicated - measured["pss_bytes"], 1024 * 1024)
            finally:
                proc.communicate("\n", timeout=10)
                self.assertEqual(proc.returncode, 0)

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

    def test_native_tree_excludes_controller_siblings_and_reused_pid(self):
        processes = {1: {"parent": 0, "starttime": "1"},
                     10: {"parent": 1, "starttime": "2"},
                     11: {"parent": 10, "starttime": "3"},
                     12: {"parent": 11, "starttime": "4"},
                     20: {"parent": 1, "starttime": "5"}}
        self.assertEqual(process_tree({"pid": 10, "starttime": "2"}, processes), [10, 11, 12])
        self.assertEqual(process_tree({"pid": 10, "starttime": "1"}, processes), [])
        self.assertEqual(process_tree({"pid": 99, "starttime": "2"}, processes), [])

    def test_parallel_transfers_excluded(self):
        events = [(0, "run_start", 0), (10, "run_start", 1),
                  (20, "callback_start", 0), (30, "callback_start", 1),
                  (80, "callback_end", 0), (90, "result", 0),
                  (100, "callback_end", 1), (110, "result", 1)]
        record = {"workload": "one", "start_ns": 1000, "wall_ns": 120,
                  "events": [{"event": kind, "id": id, "observed_ns": ns} for ns, kind, id in events]}
        self.assertEqual(callback_windows(record), [(1030, 1080), (1090, 1100)])
        samples = []
        for start, end in [(1040, 1060), (1070, 1085), (1082, 1088), (1093, 1098)]:
            samples.append({"begin_ns": start, "end_ns": end, "engine_bytes": 50, "engine_pss_bytes": 30,
                            "workloads": {"one": {"bytes": 200, "resident": {
                                "pss_bytes": 150, "sentry_runtime_pss_bytes": 10, "sentry_guest_bytes": 100,
                                "pids": [1, 2], "mm_groups": [[1, 2]]}}}})
        docker = callback_memory({"records": [record], "backend": "docker"}, samples)
        self.assertEqual(docker["bytes"]["with_docker_services_cgroup_bytes"]["p50"], 250)
        self.assertEqual(docker["bytes"]["with_runtime_pss_bytes"]["p50"], 180)
        for sample in samples:
            del sample["workloads"]["one"]["bytes"]
        result = callback_memory({"records": [record], "backend": "sandbox"}, samples)
        self.assertEqual(result["samples"], 2)
        self.assertEqual(result["errors"], [])
        self.assertEqual(result["bytes"]["sentry_guest_bytes"]["p50"], 100)
        self.assertNotIn("with_docker_services_cgroup_bytes", result["bytes"])
        self.assertNotIn("mm_groups", result["bytes"])
        self.assertEqual(result["bytes"]["with_runtime_pss_bytes"]["p50"], 150)
        fc = callback_memory({"records": [record], "backend": "firecracker"}, samples)
        self.assertEqual(fc["bytes"]["with_runtime_pss_bytes"]["p50"], 150)

    def test_missing_samples_fail_measurement(self):
        result = callback_memory({"records": [], "backend": "sandbox"}, [])
        self.assertEqual(result["samples"], 0)
        self.assertTrue(result["errors"])


if __name__ == "__main__":
    unittest.main()
