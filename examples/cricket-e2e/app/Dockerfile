# syntax=docker/dockerfile:1
FROM nvidia/cuda:12.8.1-devel-ubuntu22.04@sha256:a99a1860ba8e2916e5c3e73b72ec4c4301653a84586e05bfc9a2aa2d58027e97 AS builder

WORKDIR /src
COPY remote_kernel_test.cu .
RUN mkdir -p /out \
    && nvcc \
    -cudart shared \
    -arch=sm_120 \
    remote_kernel_test.cu \
    -o /out/remote-kernel-test

FROM nvidia/cuda:12.8.1-runtime-ubuntu22.04@sha256:4a801ef9232d2b05e69df4eb8aa054dbbe2824e5499e1e6e857320bb01ac41a9

RUN apt-get update \
    && apt-get install -y --no-install-recommends libelf1 libzstd1 zlib1g \
    && rm -rf /var/lib/apt/lists/*

COPY --from=builder /out/remote-kernel-test /opt/workload/run
USER 65532:65532
ENTRYPOINT ["/opt/workload/run"]
