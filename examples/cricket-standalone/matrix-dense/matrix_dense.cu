#include <cuda_runtime.h>

#include <algorithm>
#include <chrono>
#include <cstdint>
#include <cstdio>
#include <cstdlib>
#include <random>
#include <vector>

namespace {

constexpr int kSize = 512;
constexpr int kTile = 16;
constexpr int kSample = 4;

inline void check_cuda(cudaError_t error, const char *operation) {
    if (error != cudaSuccess) {
        std::fprintf(stderr, "CUDA failure in %s: %s\n",
                     operation, cudaGetErrorString(error));
        std::exit(10);
    }
}

__global__ void tiled_matmul(const int *x, const int *weights, int *product,
                             int size) {
    __shared__ int tile_x[kTile][kTile];
    __shared__ int tile_w[kTile][kTile];

    const int row = blockIdx.y * kTile + threadIdx.y;
    const int col = blockIdx.x * kTile + threadIdx.x;
    int accumulator = 0;

    for (int tile = 0; tile < (size + kTile - 1) / kTile; ++tile) {
        const int x_col = tile * kTile + threadIdx.x;
        const int w_row = tile * kTile + threadIdx.y;
        tile_x[threadIdx.y][threadIdx.x] =
            row < size && x_col < size ? x[row * size + x_col] : 0;
        tile_w[threadIdx.y][threadIdx.x] =
            w_row < size && col < size ? weights[w_row * size + col] : 0;
        __syncthreads();

#pragma unroll
        for (int inner = 0; inner < kTile; ++inner) {
            accumulator += tile_x[threadIdx.y][inner]
                * tile_w[inner][threadIdx.x];
        }
        __syncthreads();
    }

    if (row < size && col < size) {
        product[row * size + col] = accumulator;
    }
}

__global__ void add_bias_relu(const int *product, const int *bias, int *output,
                              int size) {
    const int index = blockIdx.x * blockDim.x + threadIdx.x;
    const int count = size * size;
    if (index >= count) {
        return;
    }
    const int column = index % size;
    const int value = product[index] + bias[column];
    output[index] = value > 0 ? value : 0;
}

uint64_t fnv1a(const std::vector<int> &values) {
    uint64_t hash = 1469598103934665603ULL;
    for (int value : values) {
        const uint32_t bits = static_cast<uint32_t>(value);
        for (int shift = 0; shift < 32; shift += 8) {
            hash ^= static_cast<unsigned char>((bits >> shift) & 0xffU);
            hash *= 1099511628211ULL;
        }
    }
    return hash;
}

void print_matrix_sample(const char *name, const std::vector<int> &matrix,
                         int size) {
    std::printf("%s top-left %dx%d:\n", name, kSample, kSample);
    for (int row = 0; row < kSample; ++row) {
        std::printf("  [");
        for (int col = 0; col < kSample; ++col) {
            std::printf("%6d%s", matrix[row * size + col],
                        col + 1 == kSample ? "" : ",");
        }
        std::printf("]\n");
    }
}

void cpu_dense_reference(const std::vector<int> &x,
                         const std::vector<int> &weights,
                         const std::vector<int> &bias,
                         std::vector<int> *output) {
    for (int row = 0; row < kSize; ++row) {
        for (int col = 0; col < kSize; ++col) {
            int accumulator = 0;
            for (int inner = 0; inner < kSize; ++inner) {
                accumulator += x[row * kSize + inner]
                    * weights[inner * kSize + col];
            }
            const int value = accumulator + bias[col];
            (*output)[row * kSize + col] = value > 0 ? value : 0;
        }
    }
}

void write_report(const char *path, uint32_t seed, double remote_ms,
                  double cpu_ms, uint64_t gpu_hash, uint64_t cpu_hash,
                  std::size_t mismatches, int max_difference,
                  const std::vector<int> &output) {
    if (path == nullptr || path[0] == '\0') {
        return;
    }
    FILE *file = std::fopen(path, "w");
    if (file == nullptr) {
        std::fprintf(stderr, "could not write report: %s\n", path);
        return;
    }
    std::fprintf(file, "demo=remote-dense-layer\n");
    std::fprintf(file, "formula=Y=ReLU(X*W+bias)\n");
    std::fprintf(file, "matrix_size=%d\n", kSize);
    std::fprintf(file, "seed=%u\n", seed);
    std::fprintf(file, "remote_end_to_end_ms=%.3f\n", remote_ms);
    std::fprintf(file, "cpu_reference_ms=%.3f\n", cpu_ms);
    std::fprintf(file, "gpu_hash=%016llx\n",
                 static_cast<unsigned long long>(gpu_hash));
    std::fprintf(file, "cpu_hash=%016llx\n",
                 static_cast<unsigned long long>(cpu_hash));
    std::fprintf(file, "mismatches=%zu\n", mismatches);
    std::fprintf(file, "max_absolute_difference=%d\n", max_difference);
    std::fprintf(file, "verification=%s\n",
                 mismatches == 0 ? "PASS" : "FAIL");
    for (int row = 0; row < kSample; ++row) {
        std::fprintf(file, "sample_row_%d=", row);
        for (int col = 0; col < kSample; ++col) {
            std::fprintf(file, "%d%s", output[row * kSize + col],
                         col + 1 == kSample ? "\n" : ",");
        }
    }
    std::fclose(file);
}

}  // namespace

int main() {
    const char *remote = std::getenv("REMOTE_GPU_ADDRESS");
    const char *seed_text = std::getenv("MATRIX_SEED");
    const uint32_t seed = seed_text == nullptr
        ? 20260820U
        : static_cast<uint32_t>(std::strtoul(seed_text, nullptr, 10));
    const int element_count = kSize * kSize;
    const std::size_t matrix_bytes = sizeof(int) * element_count;
    const std::size_t bias_bytes = sizeof(int) * kSize;

    std::printf("demo marker: remote-dense-layer\n");
    std::printf("formula: Y = ReLU(X x W + bias)\n");
    std::printf("injected remote address: %s\n",
                remote == nullptr ? "(unset)" : remote);
    std::printf("matrix dimensions: X=%dx%d W=%dx%d Y=%dx%d\n",
                kSize, kSize, kSize, kSize, kSize, kSize);
    std::printf("local input seed: %u\n", seed);
    std::printf("multiply-accumulate count: %llu\n",
                static_cast<unsigned long long>(kSize)
                    * kSize * kSize);

    std::vector<int> x(element_count);
    std::vector<int> weights(element_count);
    std::vector<int> bias(kSize);
    std::vector<int> gpu_output(element_count);
    std::vector<int> cpu_output(element_count);

    std::mt19937 generator(seed);
    std::uniform_int_distribution<int> x_distribution(-3, 3);
    std::uniform_int_distribution<int> weight_distribution(-2, 2);
    std::uniform_int_distribution<int> bias_distribution(-7, 7);
    for (int &value : x) {
        value = x_distribution(generator);
    }
    for (int &value : weights) {
        value = weight_distribution(generator);
    }
    for (int &value : bias) {
        value = bias_distribution(generator);
    }

    std::printf("local X checksum: %016llx\n",
                static_cast<unsigned long long>(fnv1a(x)));
    std::printf("local W checksum: %016llx\n",
                static_cast<unsigned long long>(fnv1a(weights)));
    print_matrix_sample("X", x, kSize);
    print_matrix_sample("W", weights, kSize);

    int device_count = 0;
    check_cuda(cudaGetDeviceCount(&device_count), "cudaGetDeviceCount");
    std::printf("device count: %d\n", device_count);
    if (device_count < 1) {
        return 1;
    }

    int *device_x = nullptr;
    int *device_weights = nullptr;
    int *device_bias = nullptr;
    int *device_product = nullptr;
    int *device_output = nullptr;
    check_cuda(cudaMalloc(&device_x, matrix_bytes), "cudaMalloc X");
    check_cuda(cudaMalloc(&device_weights, matrix_bytes), "cudaMalloc W");
    check_cuda(cudaMalloc(&device_bias, bias_bytes), "cudaMalloc bias");
    check_cuda(cudaMalloc(&device_product, matrix_bytes), "cudaMalloc product");
    check_cuda(cudaMalloc(&device_output, matrix_bytes), "cudaMalloc output");

    const auto remote_started = std::chrono::steady_clock::now();
    check_cuda(cudaMemcpy(device_x, x.data(), matrix_bytes,
                          cudaMemcpyHostToDevice), "cudaMemcpy X H2D");
    check_cuda(cudaMemcpy(device_weights, weights.data(), matrix_bytes,
                          cudaMemcpyHostToDevice), "cudaMemcpy W H2D");
    check_cuda(cudaMemcpy(device_bias, bias.data(), bias_bytes,
                          cudaMemcpyHostToDevice), "cudaMemcpy bias H2D");

    const dim3 block_2d(kTile, kTile);
    const dim3 grid_2d((kSize + kTile - 1) / kTile,
                       (kSize + kTile - 1) / kTile);
    tiled_matmul<<<grid_2d, block_2d>>>(
        device_x, device_weights, device_product, kSize);
    check_cuda(cudaGetLastError(), "tiled_matmul launch");

    const int threads = 256;
    const int blocks = (element_count + threads - 1) / threads;
    add_bias_relu<<<blocks, threads>>>(
        device_product, device_bias, device_output, kSize);
    check_cuda(cudaGetLastError(), "add_bias_relu launch");
    check_cuda(cudaDeviceSynchronize(), "dense layer synchronize");
    check_cuda(cudaMemcpy(gpu_output.data(), device_output, matrix_bytes,
                          cudaMemcpyDeviceToHost), "cudaMemcpy Y D2H");
    const auto remote_finished = std::chrono::steady_clock::now();

    const auto cpu_started = std::chrono::steady_clock::now();
    cpu_dense_reference(x, weights, bias, &cpu_output);
    const auto cpu_finished = std::chrono::steady_clock::now();

    std::size_t mismatches = 0;
    int max_difference = 0;
    std::size_t positive_outputs = 0;
    for (int index = 0; index < element_count; ++index) {
        const int difference = std::abs(gpu_output[index] - cpu_output[index]);
        max_difference = std::max(max_difference, difference);
        if (difference != 0) {
            if (mismatches < 5) {
                std::printf("mismatch[%d]: gpu=%d cpu=%d\n",
                            index, gpu_output[index], cpu_output[index]);
            }
            ++mismatches;
        }
        if (gpu_output[index] > 0) {
            ++positive_outputs;
        }
    }

    const double remote_ms = std::chrono::duration<double, std::milli>(
        remote_finished - remote_started).count();
    const double cpu_ms = std::chrono::duration<double, std::milli>(
        cpu_finished - cpu_started).count();
    const uint64_t gpu_hash = fnv1a(gpu_output);
    const uint64_t cpu_hash = fnv1a(cpu_output);
    const std::size_t h2d_bytes = matrix_bytes * 2 + bias_bytes;

    std::printf("kernel stages: tiled_matmul -> add_bias_relu\n");
    std::printf("H2D bytes: %zu\n", h2d_bytes);
    std::printf("D2H bytes: %zu\n", matrix_bytes);
    std::printf("Cricket remote end-to-end ms: %.3f\n", remote_ms);
    std::printf("local CPU reference ms: %.3f\n", cpu_ms);
    std::printf("GPU result checksum: %016llx\n",
                static_cast<unsigned long long>(gpu_hash));
    std::printf("CPU result checksum: %016llx\n",
                static_cast<unsigned long long>(cpu_hash));
    std::printf("positive outputs: %zu / %d\n",
                positive_outputs, element_count);
    std::printf("mismatched elements: %zu / %d\n", mismatches, element_count);
    std::printf("maximum absolute difference: %d\n", max_difference);
    print_matrix_sample("Y", gpu_output, kSize);
    std::printf("verification: %s\n", mismatches == 0 ? "PASS" : "FAIL");

    write_report(std::getenv("MATRIX_REPORT_PATH"), seed, remote_ms, cpu_ms,
                 gpu_hash, cpu_hash, mismatches, max_difference, gpu_output);

    cudaFree(device_output);
    cudaFree(device_product);
    cudaFree(device_bias);
    cudaFree(device_weights);
    cudaFree(device_x);
    return mismatches == 0 ? 0 : 8;
}
