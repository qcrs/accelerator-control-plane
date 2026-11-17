#!/usr/bin/env bash
set -eo pipefail

task="$DEMO_TASK"
if [ -z "$task" ]; then
  task="image-pipeline"
fi

case "$task" in
  image-pipeline)
    exec /opt/workload/image-pipeline
    ;;
  batch-resize)
    exec /opt/workload/batch-resize
    ;;
  image-segmentation)
    exec /opt/workload/image-segmentation
    ;;
  *)
    echo "unknown DEMO_TASK: $task" >&2
    echo "expected: image-pipeline, batch-resize, image-segmentation" >&2
    exit 64
    ;;
esac
