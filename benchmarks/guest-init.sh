#!/bin/sh
# Firecracker starts the same image payload as Docker, as the same UID.
mount -t proc proc /proc
mount -t sysfs sysfs /sys
mount -t devtmpfs devtmpfs /dev
mount -t tmpfs -o mode=1777,size=256m,exec tmpfs /tmp
mount -t tmpfs -o mode=1777,size=256m,exec tmpfs /work
export GLIBC_TUNABLES=glibc.pthread.rseq=0 LANG=C LC_ALL=C HOME=/work TMPDIR=/tmp MAKEFLAGS=-j1
setpriv --reuid=1000 --regid=1000 --clear-groups /opt/benchmark/formula-bench -backend=direct
status=$?
echo "BENCH:{\"event\":\"guest_exit\",\"code\":$status}"
sync
reboot -f
