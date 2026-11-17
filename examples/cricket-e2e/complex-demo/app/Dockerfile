# syntax=docker/dockerfile:1
FROM nvidia/cuda:12.8.1-devel-ubuntu22.04@sha256:a99a1860ba8e2916e5c3e73b72ec4c4301653a84586e05bfc9a2aa2d58027e97 AS builder

WORKDIR /src
COPY image_pipeline.cu batch_resize.cu image_segmentation.cu ./
RUN mkdir -p /out \
    && nvcc -O3 -std=c++14 -cudart shared -arch=sm_120 image_pipeline.cu -o /out/image-pipeline \
    && nvcc -O3 -std=c++14 -cudart shared -arch=sm_120 batch_resize.cu -o /out/batch-resize \
    && nvcc -O3 -std=c++14 -cudart shared -arch=sm_120 image_segmentation.cu -o /out/image-segmentation

FROM nvidia/cuda:12.8.1-runtime-ubuntu22.04@sha256:4a801ef9232d2b05e69df4eb8aa054dbbe2824e5499e1e6e857320bb01ac41a9

RUN apt-get update \
    && apt-get install -y --no-install-recommends libelf1 libzstd1 zlib1g \
    && rm -rf /var/lib/apt/lists/*

COPY --from=builder /out/image-pipeline /opt/workload/image-pipeline
COPY --from=builder /out/batch-resize /opt/workload/batch-resize
COPY --from=builder /out/image-segmentation /opt/workload/image-segmentation
COPY entrypoint.sh /usr/local/bin/complex-image-demo
RUN chmod 0755 /usr/local/bin/complex-image-demo \
    && chown -R 65532:65532 /opt/workload

USER 65532:65532
ENTRYPOINT ["/usr/local/bin/complex-image-demo"]
