#include <cuda_runtime.h>

#include <chrono>
#include <cmath>
#include <cstdio>
#include <cstdlib>
#include <cstdint>

namespace {

constexpr int kWidth = 1024;
constexpr int kHeight = 768;
constexpr int kPixels = kWidth * kHeight;

inline void check_cuda(cudaError_t error, const char *operation) {
    if (error != cudaSuccess) {
        std::fprintf(stderr, "CUDA failure in %s: %s\n",
                     operation, cudaGetErrorString(error));
        std::exit(10);
    }
}

__device__ __forceinline__ int clamp_index(int value, int limit) {
    return value < 0 ? 0 : (value >= limit ? limit - 1 : value);
}

__global__ void rgba_to_gray(const uchar4 *input, float *gray, int count) {
    int index = blockIdx.x * blockDim.x + threadIdx.x;
    if (index >= count) {
        return;
    }
    uchar4 pixel = input[index];
    gray[index] = 0.299f * static_cast<float>(pixel.x)
        + 0.587f * static_cast<float>(pixel.y)
        + 0.114f * static_cast<float>(pixel.z);
}

__global__ void gaussian_blur_3x3(const float *gray, float *blurred,
                                  int width, int height) {
    int x = blockIdx.x * blockDim.x + threadIdx.x;
    int y = blockIdx.y * blockDim.y + threadIdx.y;
    if (x >= width || y >= height) {
        return;
    }

    constexpr float weights[3][3] = {
        {1.0f, 2.0f, 1.0f},
        {2.0f, 4.0f, 2.0f},
        {1.0f, 2.0f, 1.0f},
    };
    float sum = 0.0f;
    for (int ky = -1; ky <= 1; ++ky) {
        for (int kx = -1; kx <= 1; ++kx) {
            int sx = clamp_index(x + kx, width);
            int sy = clamp_index(y + ky, height);
            sum += gray[sy * width + sx] * weights[ky + 1][kx + 1];
        }
    }
    blurred[y * width + x] = sum / 16.0f;
}

__global__ void sobel_edges(const float *blurred, unsigned char *edges,
                            int width, int height) {
    int x = blockIdx.x * blockDim.x + threadIdx.x;
    int y = blockIdx.y * blockDim.y + threadIdx.y;
    if (x >= width || y >= height) {
        return;
    }

    constexpr int sobel_x[3][3] = {
        {-1, 0, 1},
        {-2, 0, 2},
        {-1, 0, 1},
    };
    constexpr int sobel_y[3][3] = {
        {-1, -2, -1},
        {0, 0, 0},
        {1, 2, 1},
    };
    float gx = 0.0f;
    float gy = 0.0f;
    for (int ky = -1; ky <= 1; ++ky) {
        for (int kx = -1; kx <= 1; ++kx) {
            int sx = clamp_index(x + kx, width);
            int sy = clamp_index(y + ky, height);
            float value = blurred[sy * width + sx];
            gx += value * static_cast<float>(sobel_x[ky + 1][kx + 1]);
            gy += value * static_cast<float>(sobel_y[ky + 1][kx + 1]);
        }
    }
    float magnitude = sqrtf(gx * gx + gy * gy);
    edges[y * width + x] = static_cast<unsigned char>(
        fminf(255.0f, fmaxf(0.0f, magnitude)));
}

__global__ void histogram_256(const unsigned char *values, int *histogram,
                              int count) {
    int index = blockIdx.x * blockDim.x + threadIdx.x;
    if (index < count) {
        atomicAdd(&histogram[values[index]], 1);
    }
}

void make_input(uchar4 *input) {
    for (int y = 0; y < kHeight; ++y) {
        for (int x = 0; x < kWidth; ++x) {
            int index = y * kWidth + x;
            unsigned char wave = static_cast<unsigned char>(
                (x * 13 + y * 7 + ((x / 32 + y / 32) % 2) * 61) % 256);
            input[index] = make_uchar4(
                static_cast<unsigned char>((wave + x / 4) % 256),
                static_cast<unsigned char>((wave + y / 3) % 256),
                static_cast<unsigned char>((wave + x / 8 + y / 5) % 256),
                255);
        }
    }
}

void cpu_reference(const uchar4 *input, unsigned char *edges, int *histogram) {
    float *gray = static_cast<float *>(std::malloc(sizeof(float) * kPixels));
    float *blurred = static_cast<float *>(std::malloc(sizeof(float) * kPixels));
    if (gray == nullptr || blurred == nullptr) {
        std::fprintf(stderr, "host allocation failed\n");
        std::exit(11);
    }
    for (int i = 0; i < kPixels; ++i) {
        gray[i] = 0.299f * static_cast<float>(input[i].x)
            + 0.587f * static_cast<float>(input[i].y)
            + 0.114f * static_cast<float>(input[i].z);
    }
    for (int y = 0; y < kHeight; ++y) {
        for (int x = 0; x < kWidth; ++x) {
            constexpr float weights[3][3] = {
                {1.0f, 2.0f, 1.0f},
                {2.0f, 4.0f, 2.0f},
                {1.0f, 2.0f, 1.0f},
            };
            float sum = 0.0f;
            for (int ky = -1; ky <= 1; ++ky) {
                for (int kx = -1; kx <= 1; ++kx) {
                    int sx = x + kx < 0 ? 0 : (x + kx >= kWidth ? kWidth - 1 : x + kx);
                    int sy = y + ky < 0 ? 0 : (y + ky >= kHeight ? kHeight - 1 : y + ky);
                    sum += gray[sy * kWidth + sx] * weights[ky + 1][kx + 1];
                }
            }
            blurred[y * kWidth + x] = sum / 16.0f;
        }
    }
    constexpr int sobel_x[3][3] = {
        {-1, 0, 1},
        {-2, 0, 2},
        {-1, 0, 1},
    };
    constexpr int sobel_y[3][3] = {
        {-1, -2, -1},
        {0, 0, 0},
        {1, 2, 1},
    };
    for (int y = 0; y < kHeight; ++y) {
        for (int x = 0; x < kWidth; ++x) {
            float gx = 0.0f;
            float gy = 0.0f;
            for (int ky = -1; ky <= 1; ++ky) {
                for (int kx = -1; kx <= 1; ++kx) {
                    int sx = x + kx < 0 ? 0 : (x + kx >= kWidth ? kWidth - 1 : x + kx);
                    int sy = y + ky < 0 ? 0 : (y + ky >= kHeight ? kHeight - 1 : y + ky);
                    float value = blurred[sy * kWidth + sx];
                    gx += value * static_cast<float>(sobel_x[ky + 1][kx + 1]);
                    gy += value * static_cast<float>(sobel_y[ky + 1][kx + 1]);
                }
            }
            unsigned char edge = static_cast<unsigned char>(
                fminf(255.0f, fmaxf(0.0f, sqrtf(gx * gx + gy * gy))));
            edges[y * kWidth + x] = edge;
            histogram[edge]++;
        }
    }
    std::free(gray);
    std::free(blurred);
}

void write_pgm(const char *path, const unsigned char *pixels) {
    FILE *file = std::fopen(path, "wb");
    if (file == nullptr) {
        std::fprintf(stderr, "could not write output image: %s\n", path);
        return;
    }
    std::fprintf(file, "P5\n%d %d\n255\n", kWidth, kHeight);
    std::fwrite(pixels, 1, kPixels, file);
    std::fclose(file);
}

}  // namespace

int main() {
    const char *remote = std::getenv("REMOTE_GPU_ADDRESS");
    std::printf("demo marker: image-pipeline\n");
    std::printf("injected remote address: %s\n", remote == nullptr ? "(unset)" : remote);
    std::printf("input: RGBA %dx%d, stages: grayscale -> gaussian3x3 -> sobel -> histogram\n",
                kWidth, kHeight);

    int device_count = 0;
    check_cuda(cudaGetDeviceCount(&device_count), "cudaGetDeviceCount");
    std::printf("device count: %d\n", device_count);
    if (device_count < 1) {
        return 1;
    }

    uchar4 *host_input = static_cast<uchar4 *>(std::malloc(sizeof(uchar4) * kPixels));
    unsigned char *host_edges = static_cast<unsigned char *>(std::malloc(kPixels));
    unsigned char *reference_edges = static_cast<unsigned char *>(std::malloc(kPixels));
    int *host_histogram = static_cast<int *>(std::calloc(256, sizeof(int)));
    int *reference_histogram = static_cast<int *>(std::calloc(256, sizeof(int)));
    make_input(host_input);

    uchar4 *device_input = nullptr;
    float *device_gray = nullptr;
    float *device_blurred = nullptr;
    unsigned char *device_edges = nullptr;
    int *device_histogram = nullptr;
    check_cuda(cudaMalloc(&device_input, sizeof(uchar4) * kPixels), "cudaMalloc input");
    check_cuda(cudaMalloc(&device_gray, sizeof(float) * kPixels), "cudaMalloc gray");
    check_cuda(cudaMalloc(&device_blurred, sizeof(float) * kPixels), "cudaMalloc blurred");
    check_cuda(cudaMalloc(&device_edges, kPixels), "cudaMalloc edges");
    check_cuda(cudaMalloc(&device_histogram, sizeof(int) * 256), "cudaMalloc histogram");

    auto started = std::chrono::steady_clock::now();
    check_cuda(cudaMemcpy(device_input, host_input, sizeof(uchar4) * kPixels,
                          cudaMemcpyHostToDevice), "cudaMemcpy input");
    check_cuda(cudaMemset(device_histogram, 0, sizeof(int) * 256),
               "cudaMemset histogram");

    dim3 block_1d(256);
    dim3 grid_1d((kPixels + block_1d.x - 1) / block_1d.x);
    rgba_to_gray<<<grid_1d, block_1d>>>(device_input, device_gray, kPixels);
    check_cuda(cudaGetLastError(), "rgba_to_gray launch");

    dim3 block_2d(16, 16);
    dim3 grid_2d((kWidth + block_2d.x - 1) / block_2d.x,
                 (kHeight + block_2d.y - 1) / block_2d.y);
    gaussian_blur_3x3<<<grid_2d, block_2d>>>(
        device_gray, device_blurred, kWidth, kHeight);
    check_cuda(cudaGetLastError(), "gaussian_blur_3x3 launch");
    sobel_edges<<<grid_2d, block_2d>>>(
        device_blurred, device_edges, kWidth, kHeight);
    check_cuda(cudaGetLastError(), "sobel_edges launch");
    histogram_256<<<grid_1d, block_1d>>>(device_edges, device_histogram, kPixels);
    check_cuda(cudaGetLastError(), "histogram launch");
    check_cuda(cudaDeviceSynchronize(), "pipeline synchronize");

    check_cuda(cudaMemcpy(host_edges, device_edges, kPixels,
                          cudaMemcpyDeviceToHost), "cudaMemcpy edges");
    check_cuda(cudaMemcpy(host_histogram, device_histogram, sizeof(int) * 256,
                          cudaMemcpyDeviceToHost), "cudaMemcpy histogram");
    auto finished = std::chrono::steady_clock::now();

    cpu_reference(host_input, reference_edges, reference_histogram);
    int failures = 0;
    uint64_t checksum = 0;
    int histogram_total = 0;
    for (int i = 0; i < kPixels; ++i) {
        checksum += host_edges[i];
        if (std::abs(static_cast<int>(host_edges[i])
                     - static_cast<int>(reference_edges[i])) > 1) {
            ++failures;
        }
    }
    for (int bin = 0; bin < 256; ++bin) {
        histogram_total += host_histogram[bin];
        if (host_histogram[bin] != reference_histogram[bin]) {
            ++failures;
        }
    }
    double elapsed_ms = std::chrono::duration<double, std::milli>(
        finished - started).count();
    const char *output = std::getenv("OUTPUT_PATH");
    write_pgm(output == nullptr ? "/tmp/image-pipeline-edge.pgm" : output, host_edges);
    std::printf("pipeline wall time ms: %.3f\n", elapsed_ms);
    std::printf("edge checksum: %llu\n",
                static_cast<unsigned long long>(checksum));
    std::printf("histogram total: %d\n", histogram_total);
    std::printf("verification failures: %d\n", failures);

    cudaFree(device_histogram);
    cudaFree(device_edges);
    cudaFree(device_blurred);
    cudaFree(device_gray);
    cudaFree(device_input);
    std::free(reference_histogram);
    std::free(host_histogram);
    std::free(reference_edges);
    std::free(host_edges);
    std::free(host_input);
    return failures == 0 ? 0 : 8;
}
