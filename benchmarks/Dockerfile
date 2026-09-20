FROM golang:1.26.6-bookworm AS build
WORKDIR /src
COPY . .
RUN bash benchmarks/build.sh /out

FROM golang:1.26.6-bookworm
COPY --from=build /out /opt/benchmark
COPY benchmarks/guest-init.sh /sbin/benchmark-init
RUN chmod 755 /sbin/benchmark-init && mkdir /work && chmod 1777 /work
ENV GLIBC_TUNABLES=glibc.pthread.rseq=0 LANG=C LC_ALL=C HOME=/work TMPDIR=/tmp MAKEFLAGS=-j1
USER 1000:1000
ENTRYPOINT ["/opt/benchmark/formula-bench"]
