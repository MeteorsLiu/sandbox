#!/bin/sh
# Firecracker starts the same image payload as Docker, as the same UID.
mount -t proc proc /proc
mount -t sysfs sysfs /sys
mount -t tmpfs -o mode=1777,size=256m,exec tmpfs /tmp
mount -t tmpfs -o mode=1777,size=256m,exec tmpfs /work
export GLIBC_TUNABLES=glibc.pthread.rseq=0 LANG=C LC_ALL=C HOME=/work TMPDIR=/tmp MAKEFLAGS=-j1
setpriv --reuid=1000 --regid=1000 --clear-groups /opt/benchmark/formula-bench -backend=direct
status=$?
if test "$status" -ne 0; then
    find /work -name configure.log -print -exec cat '{}' ';'
fi
echo "BENCH:{\"event\":\"guest_exit\",\"code\":$status}"
exec /opt/benchmark/reboot
