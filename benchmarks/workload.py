"""Verify the compilation workload of the pinned zlib Formula."""
import collections
import hashlib
import json
import pathlib
import shlex


def audit_workload(log, count, events):
    library = "adler32 crc32 deflate infback inffast inflate inftrees trees zutil compress uncompr gzclose gzlib gzread gzwrite".split()
    expected = collections.Counter((name + ".o", name + ".c") for name in library)
    expected.update({("example.o", "test/example.c"): 1,
                     ("minigzip.o", "test/minigzip.c"): 1,
                     ("example64.o", "test/example.c"): 1,
                     ("minigzip64.o", "test/minigzip.c"): 1})
    jobs = {str(i): collections.Counter() for i in range(count)}
    commands = {str(i): [] for i in range(count)}
    configure, archives, errors = 0, 0, []
    for line in log.splitlines():
        if line == "Building static library libz.a version 1.3.1 with gcc.":
            configure += 1
        if not line.startswith(("gcc ", "ar ")):
            continue
        args = shlex.split(line)
        if args[:3] == ["ar", "rc", "libz.a"]:
            archives += 1
            if args[3:] != [name + ".o" for name in library]:
                errors.append("archive members differ from the pinned zlib workload")
        if args[0] != "gcc" or "-c" not in args:
            continue
        source = pathlib.PurePosixPath(args[-1])
        parts = source.parts
        if len(parts) < 5 or parts[:2] != ("/", "work") or not parts[2].startswith("job-") or parts[3] != "source":
            errors.append("unexpected compilation source: " + str(source))
            continue
        job = parts[2].removeprefix("job-")
        if job not in jobs or "-o" not in args or args.index("-o") + 1 >= len(args):
            errors.append("unexpected compilation command: " + line)
            continue
        jobs[job][(args[args.index("-o") + 1], "/".join(parts[4:]))] += 1
        commands[job].append([arg.replace("/work/job-" + job + "/", "/work/job/", 1) for arg in args])
    callbacks = sum(event["event"] == "callback_start" for event in events)
    if callbacks != count or configure != count or archives != count:
        errors.append(f"expected {count} callbacks/configures/archives, got {callbacks}/{configure}/{archives}")
    for job, compiled in jobs.items():
        if compiled != expected:
            errors.append(f"job {job}: missing {list((expected-compiled).elements())}, extra {list((compiled-expected).elements())}")
    digests = {job: hashlib.sha256(json.dumps(sorted(args)).encode()).hexdigest() for job, args in commands.items()}
    if len(set(digests.values())) != 1:
        errors.append("compiler commands differ between jobs")
    return {"callbacks": callbacks, "configures": configure, "archives": archives,
            "compilations": {job: sum(compiled.values()) for job, compiled in jobs.items()},
            "compiler_commands_sha256": digests, "errors": errors}
