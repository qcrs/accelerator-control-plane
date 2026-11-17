#include <cuda_runtime.h>
#include <stdio.h>
#include <stdlib.h>

__global__ void add_one(int *data) {
    int i = blockIdx.x * blockDim.x + threadIdx.x;
    if (i < 256) {
        data[i] += 1;
    }
}

int main(void) {
    const char *demo = getenv("CRICKET_DEMO");
    const char *remote = getenv("REMOTE_GPU_ADDRESS");
    printf("demo marker: %s\n", demo != NULL ? demo : "(unset)");
    printf("injected remote address: %s\n", remote != NULL ? remote : "(unset)");

    int count = 0;
    cudaError_t err = cudaGetDeviceCount(&count);
    printf("device count: err=%d count=%d\n", (int)err, count);
    if (err != cudaSuccess || count < 1) return 1;

    int host_data[256];
    for (int i = 0; i < 256; ++i) {
        host_data[i] = i;
    }

    int *device_data = NULL;
    err = cudaMalloc(&device_data, sizeof(host_data));
    if (err != cudaSuccess) return 2;

    err = cudaMemcpy(
        device_data,
        host_data,
        sizeof(host_data),
        cudaMemcpyHostToDevice);
    if (err != cudaSuccess) return 3;

    add_one<<<1, 256>>>(device_data);
    err = cudaGetLastError();
    printf("kernel launch: err=%d\n", (int)err);
    if (err != cudaSuccess) return 4;

    err = cudaDeviceSynchronize();
    printf("kernel sync: err=%d\n", (int)err);
    if (err != cudaSuccess) return 5;

    err = cudaMemcpy(
        host_data,
        device_data,
        sizeof(host_data),
        cudaMemcpyDeviceToHost);
    if (err != cudaSuccess) return 6;

    int failures = 0;
    for (int i = 0; i < 256; ++i) {
        if (host_data[i] != i + 1) {
            ++failures;
        }
    }

    err = cudaFree(device_data);
    if (err != cudaSuccess) return 7;

    printf("verification failures: %d\n", failures);
    printf("sample: %d %d %d\n",
           host_data[0],
           host_data[1],
           host_data[255]);
    return failures == 0 ? 0 : 8;
}
