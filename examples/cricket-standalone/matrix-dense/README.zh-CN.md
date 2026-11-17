# Cricket 独立矩阵全连接层 Demo

该示例不使用 Kubernetes、容器或平台适配层。调用端本地生成 `X`、`W` 和 `bias`，通过 Cricket 把数据上传到远端 GPU，执行：

```text
Y = ReLU(X × W + bias)
```

矩阵规模为 `512×512`，包含 `134,217,728` 次乘加。远端 GPU 返回完整 `Y` 后，调用端 CPU 对全部 `262,144` 个输出执行相同计算并逐项比较。

构建：

```bash
./examples/cricket-standalone/matrix-dense/build-remote.sh
```

运行：

```bash
./examples/cricket-standalone/matrix-dense/run-remote.sh
```

连续运行并同步采集远端 GPU 活动：

```bash
./examples/cricket-standalone/matrix-dense/observe-remote.sh
```

验收条件：

```text
GPU result checksum == CPU result checksum
mismatched elements: 0 / 262144
maximum absolute difference: 0
verification: PASS
exit=0
```
