#include <cuda_runtime.h>

#include <chrono>
#include <cmath>
#include <cstdio>
#include <cstdlib>

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

__global__ void rgba_to_luma(const uchar4 *input, float *luma, int count) {
    int index = blockIdx.x * blockDim.x + threadIdx.x;
    if (index >= count) {
        return;
    }
    uchar4 pixel = input[index];
    luma[index] = 0.299f * static_cast<float>(pixel.x)
        + 0.587f * static_cast<float>(pixel.y)
        + 0.114f * static_cast<float>(pixel.z);
}

__global__ void gaussian_blur_5x5(const float *input, float *output,
                                  int width, int height) {
    int x = blockIdx.x * blockDim.x + threadIdx.x;
    int y = blockIdx.y * blockDim.y + threadIdx.y;
    if (x >= width || y >= height) {
        return;
    }
    constexpr float weights[5] = {1.0f, 4.0f, 6.0f, 4.0f, 1.0f};
    float sum = 0.0f;
    for (int ky = -2; ky <= 2; ++ky) {
        for (int kx = -2; kx <= 2; ++kx) {
            int sx = clamp_index(x + kx, width);
            int sy = clamp_index(y + ky, height);
            sum += input[sy * width + sx]
                * weights[ky + 2] * weights[kx + 2];
        }
    }
    output[y * width + x] = sum / 256.0f;
}

__global__ void threshold_mask(const float *blurred, unsigned char *mask,
                               int count, float threshold) {
    int index = blockIdx.x * blockDim.x + threadIdx.x;
    if (index < count) {
        mask[index] = blurred[index] >= threshold ? 1 : 0;
    }
}

__global__ void majority_filter(const unsigned char *input,
                                unsigned char *output,
                                int width, int height) {
    int x = blockIdx.x * blockDim.x + threadIdx.x;
    int y = blockIdx.y * blockDim.y + threadIdx.y;
    if (x >= width || y >= height) {
        return;
    }
    int foreground = 0;
    for (int ky = -1; ky <= 1; ++ky) {
        for (int kx = -1; kx <= 1; ++kx) {
            int sx = clamp_index(x + kx, width);
            int sy = clamp_index(y + ky, height);
            foreground += input[sy * width + sx] != 0 ? 1 : 0;
        }
    }
    output[y * width + x] = foreground >= 5 ? 1 : 0;
}

__global__ void count_foreground(const unsigned char *mask, int *count,
                                 int total) {
    int index = blockIdx.x * blockDim.x + threadIdx.x;
    if (index < total && mask[index] != 0) {
        atomicAdd(count, 1);
    }
}

void make_input(uchar4 *input) {
    for (int y = 0; y < kHeight; ++y) {
        for (int x = 0; x < kWidth; ++x) {
            int dx = x - kWidth / 2;
            int dy = y - kHeight / 2;
            bool circle = dx * dx + dy * dy < 170 * 170;
            bool rectangle = x > 120 && x < 350 && y > 500 && y < 680;
            unsigned char foreground = circle || rectangle ? 220 : 35;
            unsigned char texture = static_cast<unsigned char>((x * 5 + y * 3) % 25);
            input[y * kWidth + x] = make_uchar4(
                static_cast<unsigned char>(foreground + texture),
                static_cast<unsigned char>(foreground / 2 + texture),
                static_cast<unsigned char>(foreground / 3 + texture),
                255);
        }
    }
}

void host_reference(const uchar4 *input, unsigned char *mask,
                    int *foreground_count) {
    float *luma = static_cast<float *>(std::malloc(sizeof(float) * kPixels));
    float *blurred = static_cast<float *>(std::malloc(sizeof(float) * kPixels));
    unsigned char *initial = static_cast<unsigned char *>(
        std::malloc(kPixels));
    if (luma == nullptr || blurred == nullptr || initial == nullptr) {
        std::fprintf(stderr, "host allocation failed\n");
        std::exit(11);
    }
    for (int i = 0; i < kPixels; ++i) {
        luma[i] = 0.299f * static_cast<float>(input[i].x)
            + 0.587f * static_cast<float>(input[i].y)
            + 0.114f * static_cast<float>(input[i].z);
    }
    constexpr float weights[5] = {1.0f, 4.0f, 6.0f, 4.0f, 1.0f};
    for (int y = 0; y < kHeight; ++y) {
        for (int x = 0; x < kWidth; ++x) {
            float sum = 0.0f;
            for (int ky = -2; ky <= 2; ++ky) {
                for (int kx = -2; kx <= 2; ++kx) {
                    int sx = x + kx < 0 ? 0 : (x + kx >= kWidth ? kWidth - 1 : x + kx);
                    int sy = y + ky < 0 ? 0 : (y + ky >= kHeight ? kHeight - 1 : y + ky);
                    sum += luma[sy * kWidth + sx]
                        * weights[ky + 2] * weights[kx + 2];
                }
            }
            blurred[y * kWidth + x] = sum / 256.0f;
            initial[y * kWidth + x] = blurred[y * kWidth + x] >= 128.0f ? 1 : 0;
        }
    }
    *foreground_count = 0;
    for (int y = 0; y < kHeight; ++y) {
        for (int x = 0; x < kWidth; ++x) {
            int foreground = 0;
            for (int ky = -1; ky <= 1; ++ky) {
                for (int kx = -1; kx <= 1; ++kx) {
                    int sx = x + kx < 0 ? 0 : (x + kx >= kWidth ? kWidth - 1 : x + kx);
                    int sy = y + ky < 0 ? 0 : (y + ky >= kHeight ? kHeight - 1 : y + ky);
                    foreground += initial[sy * kWidth + sx] != 0 ? 1 : 0;
                }
            }
            mask[y * kWidth + x] = foreground >= 5 ? 1 : 0;
            *foreground_count += mask[y * kWidth + x] != 0 ? 1 : 0;
        }
    }
    std::free(initial);
    std::free(blurred);
    std::free(luma);
}

void write_pgm(const char *path, const unsigned char *mask) {
    FILE *file = std::fopen(path, "wb");
    if (file == nullptr) {
        std::fprintf(stderr, "could not write output mask: %s\n", path);
        return;
    }
    std::fprintf(file, "P5\n%d %d\n255\n", kWidth, kHeight);
    for (int i = 0; i < kPixels; ++i) {
        std::fputc(mask[i] == 0 ? 0 : 255, file);
    }
    std::fclose(file);
}

}  // namespace

int main() {
    const char *remote = std::getenv("REMOTE_GPU_ADDRESS");
    std::printf("demo marker: image-segmentation\n");
    std::printf("injected remote address: %s\n", remote == nullptr ? "(unset)" : remote);
    std::printf("pipeline: luma -> gaussian5x5 -> threshold -> majority3x3\n");

    int device_count = 0;
    check_cuda(cudaGetDeviceCount(&device_count), "cudaGetDeviceCount");
    std::printf("device count: %d\n", device_count);
    if (device_count < 1) {
        return 1;
    }

    uchar4 *host_input = static_cast<uchar4 *>(
        std::malloc(sizeof(uchar4) * kPixels));
    unsigned char *host_mask = static_cast<unsigned char *>(
        std::malloc(kPixels));
    unsigned char *reference_mask = static_cast<unsigned char *>(
        std::malloc(kPixels));
    make_input(host_input);

    uchar4 *device_input = nullptr;
    float *device_luma = nullptr;
    float *device_blurred = nullptr;
    unsigned char *device_initial = nullptr;
    unsigned char *device_mask = nullptr;
    int *device_foreground_count = nullptr;
    check_cuda(cudaMalloc(&device_input, sizeof(uchar4) * kPixels),
               "cudaMalloc input");
    check_cuda(cudaMalloc(&device_luma, sizeof(float) * kPixels),
               "cudaMalloc luma");
    check_cuda(cudaMalloc(&device_blurred, sizeof(float) * kPixels),
               "cudaMalloc blurred");
    check_cuda(cudaMalloc(&device_initial, kPixels),
               "cudaMalloc initial mask");
    check_cuda(cudaMalloc(&device_mask, kPixels),
               "cudaMalloc final mask");
    check_cuda(cudaMalloc(&device_foreground_count, sizeof(int)),
               "cudaMalloc count");

    auto started = std::chrono::steady_clock::now();
    check_cuda(cudaMemcpy(device_input, host_input, sizeof(uchar4) * kPixels,
                          cudaMemcpyHostToDevice), "cudaMemcpy input");
    check_cuda(cudaMemset(device_foreground_count, 0, sizeof(int)),
               "cudaMemset count");
    dim3 block_1d(256);
    dim3 grid_1d((kPixels + block_1d.x - 1) / block_1d.x);
    rgba_to_luma<<<grid_1d, block_1d>>>(device_input, device_luma, kPixels);
    check_cuda(cudaGetLastError(), "rgba_to_luma launch");
    dim3 block_2d(16, 16);
    dim3 grid_2d((kWidth + block_2d.x - 1) / block_2d.x,
                 (kHeight + block_2d.y - 1) / block_2d.y);
    gaussian_blur_5x5<<<grid_2d, block_2d>>>(
        device_luma, device_blurred, kWidth, kHeight);
    check_cuda(cudaGetLastError(), "gaussian_blur_5x5 launch");
    threshold_mask<<<grid_1d, block_1d>>>(
        device_blurred, device_initial, kPixels, 128.0f);
    check_cuda(cudaGetLastError(), "threshold_mask launch");
    majority_filter<<<grid_2d, block_2d>>>(
        device_initial, device_mask, kWidth, kHeight);
    check_cuda(cudaGetLastError(), "majority_filter launch");
    count_foreground<<<grid_1d, block_1d>>>(
        device_mask, device_foreground_count, kPixels);
    check_cuda(cudaGetLastError(), "count_foreground launch");
    check_cuda(cudaDeviceSynchronize(), "segmentation synchronize");
    check_cuda(cudaMemcpy(host_mask, device_mask, kPixels,
                          cudaMemcpyDeviceToHost), "cudaMemcpy mask");
    int gpu_count = 0;
    check_cuda(cudaMemcpy(&gpu_count, device_foreground_count, sizeof(int),
                          cudaMemcpyDeviceToHost), "cudaMemcpy count");
    auto finished = std::chrono::steady_clock::now();

    int reference_count = 0;
    host_reference(host_input, reference_mask, &reference_count);
    int failures = gpu_count == reference_count ? 0 : 1;
    for (int i = 0; i < kPixels; ++i) {
        if (host_mask[i] != reference_mask[i]) {
            ++failures;
        }
    }
    double elapsed_ms = std::chrono::duration<double, std::milli>(
        finished - started).count();
    const char *output = std::getenv("OUTPUT_PATH");
    write_pgm(output == nullptr ? "/tmp/image-segmentation-mask.pgm" : output,
              host_mask);
    std::printf("foreground pixels: %d (reference %d)\n",
                gpu_count, reference_count);
    std::printf("foreground ratio: %.6f\n",
                static_cast<double>(gpu_count) / static_cast<double>(kPixels));
    std::printf("segmentation wall time ms: %.3f\n", elapsed_ms);
    std::printf("verification failures: %d\n", failures);

    cudaFree(device_foreground_count);
    cudaFree(device_mask);
    cudaFree(device_initial);
    cudaFree(device_blurred);
    cudaFree(device_luma);
    cudaFree(device_input);
    std::free(reference_mask);
    std::free(host_mask);
    std::free(host_input);
    return failures == 0 ? 0 : 8;
}
