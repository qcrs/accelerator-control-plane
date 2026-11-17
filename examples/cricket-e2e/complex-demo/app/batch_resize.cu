#include <cuda_runtime.h>

#include <chrono>
#include <cmath>
#include <cstdio>
#include <cstdlib>
#include <cstdint>

namespace {

constexpr int kBatch = 4;
constexpr int kInputWidth = 640;
constexpr int kInputHeight = 480;
constexpr int kOutputWidth = 320;
constexpr int kOutputHeight = 240;
constexpr int kInputPixels = kInputWidth * kInputHeight;
constexpr int kOutputPixels = kOutputWidth * kOutputHeight;

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

__device__ __forceinline__ unsigned char interpolate_channel(
    unsigned char a, unsigned char b, unsigned char c, unsigned char d,
    float top_x, float top_y, float tx, float ty) {
    float value = top_y * (top_x * static_cast<float>(a)
        + tx * static_cast<float>(b))
        + ty * (top_x * static_cast<float>(c)
        + tx * static_cast<float>(d));
    return static_cast<unsigned char>(fminf(255.0f, fmaxf(0.0f, value + 0.5f)));
}

__device__ __forceinline__ uchar4 bilinear_sample(
    const uchar4 *input, int batch, int output_x, int output_y) {
    float source_x = (static_cast<float>(output_x) + 0.5f)
        * static_cast<float>(kInputWidth) / static_cast<float>(kOutputWidth) - 0.5f;
    float source_y = (static_cast<float>(output_y) + 0.5f)
        * static_cast<float>(kInputHeight) / static_cast<float>(kOutputHeight) - 0.5f;
    int x0 = static_cast<int>(floorf(source_x));
    int y0 = static_cast<int>(floorf(source_y));
    float tx = source_x - floorf(source_x);
    float ty = source_y - floorf(source_y);
    int x1 = x0 + 1;
    int y1 = y0 + 1;
    x0 = clamp_index(x0, kInputWidth);
    x1 = clamp_index(x1, kInputWidth);
    y0 = clamp_index(y0, kInputHeight);
    y1 = clamp_index(y1, kInputHeight);
    const uchar4 *base = input + batch * kInputPixels;
    uchar4 p00 = base[y0 * kInputWidth + x0];
    uchar4 p10 = base[y0 * kInputWidth + x1];
    uchar4 p01 = base[y1 * kInputWidth + x0];
    uchar4 p11 = base[y1 * kInputWidth + x1];
    float top_x = 1.0f - tx;
    float top_y = 1.0f - ty;
    return make_uchar4(
        interpolate_channel(p00.x, p10.x, p01.x, p11.x,
                            top_x, top_y, tx, ty),
        interpolate_channel(p00.y, p10.y, p01.y, p11.y,
                            top_x, top_y, tx, ty),
        interpolate_channel(p00.z, p10.z, p01.z, p11.z,
                            top_x, top_y, tx, ty),
        255);
}

__global__ void resize_batch(const uchar4 *input, uchar4 *output) {
    int x = blockIdx.x * blockDim.x + threadIdx.x;
    int y = blockIdx.y * blockDim.y + threadIdx.y;
    int batch = blockIdx.z;
    if (x >= kOutputWidth || y >= kOutputHeight || batch >= kBatch) {
        return;
    }
    output[batch * kOutputPixels + y * kOutputWidth + x] =
        bilinear_sample(input, batch, x, y);
}

__global__ void accumulate_stats(const uchar4 *output,
                                 unsigned long long *sums) {
    int index = blockIdx.x * blockDim.x + threadIdx.x;
    int total = kBatch * kOutputPixels;
    if (index >= total) {
        return;
    }
    int batch = index / kOutputPixels;
    uchar4 pixel = output[index];
    atomicAdd(&sums[batch * 4 + 0],
              static_cast<unsigned long long>(pixel.x));
    atomicAdd(&sums[batch * 4 + 1],
              static_cast<unsigned long long>(pixel.y));
    atomicAdd(&sums[batch * 4 + 2],
              static_cast<unsigned long long>(pixel.z));
    atomicAdd(&sums[batch * 4 + 3], 1ULL);
}

void make_input(uchar4 *input) {
    for (int batch = 0; batch < kBatch; ++batch) {
        for (int y = 0; y < kInputHeight; ++y) {
            for (int x = 0; x < kInputWidth; ++x) {
                int index = batch * kInputPixels + y * kInputWidth + x;
                unsigned char base = static_cast<unsigned char>(
                    (x * (batch + 3) + y * (batch + 5)
                     + 23 * batch + ((x / 16 + y / 16) % 2) * 47) % 256);
                input[index] = make_uchar4(
                    base,
                    static_cast<unsigned char>((base + 31 + batch * 7) % 256),
                    static_cast<unsigned char>((base + x / 5 + y / 7) % 256),
                    255);
            }
        }
    }
}

unsigned char host_interpolate(unsigned char a, unsigned char b,
                               unsigned char c, unsigned char d,
                               float top_x, float top_y, float tx, float ty) {
    float value = top_y * (top_x * static_cast<float>(a)
        + tx * static_cast<float>(b))
        + ty * (top_x * static_cast<float>(c)
        + tx * static_cast<float>(d));
    return static_cast<unsigned char>(fminf(255.0f, fmaxf(0.0f, value + 0.5f)));
}

uchar4 host_sample(const uchar4 *input, int batch, int x, int y) {
    float source_x = (static_cast<float>(x) + 0.5f)
        * static_cast<float>(kInputWidth) / static_cast<float>(kOutputWidth) - 0.5f;
    float source_y = (static_cast<float>(y) + 0.5f)
        * static_cast<float>(kInputHeight) / static_cast<float>(kOutputHeight) - 0.5f;
    int raw_x0 = static_cast<int>(floorf(source_x));
    int raw_y0 = static_cast<int>(floorf(source_y));
    float tx = source_x - floorf(source_x);
    float ty = source_y - floorf(source_y);
    int x0 = raw_x0 < 0 ? 0 : (raw_x0 >= kInputWidth ? kInputWidth - 1 : raw_x0);
    int x1 = raw_x0 + 1 < 0 ? 0 : (raw_x0 + 1 >= kInputWidth ? kInputWidth - 1 : raw_x0 + 1);
    int y0 = raw_y0 < 0 ? 0 : (raw_y0 >= kInputHeight ? kInputHeight - 1 : raw_y0);
    int y1 = raw_y0 + 1 < 0 ? 0 : (raw_y0 + 1 >= kInputHeight ? kInputHeight - 1 : raw_y0 + 1);
    const uchar4 *base = input + batch * kInputPixels;
    uchar4 p00 = base[y0 * kInputWidth + x0];
    uchar4 p10 = base[y0 * kInputWidth + x1];
    uchar4 p01 = base[y1 * kInputWidth + x0];
    uchar4 p11 = base[y1 * kInputWidth + x1];
    float top_x = 1.0f - tx;
    float top_y = 1.0f - ty;
    return make_uchar4(
        host_interpolate(p00.x, p10.x, p01.x, p11.x, top_x, top_y, tx, ty),
        host_interpolate(p00.y, p10.y, p01.y, p11.y, top_x, top_y, tx, ty),
        host_interpolate(p00.z, p10.z, p01.z, p11.z, top_x, top_y, tx, ty),
        255);
}

void write_ppm(const char *path, const uchar4 *output) {
    FILE *file = std::fopen(path, "wb");
    if (file == nullptr) {
        std::fprintf(stderr, "could not write output image: %s\n", path);
        return;
    }
    std::fprintf(file, "P6\n%d %d\n255\n", kOutputWidth, kOutputHeight);
    for (int i = 0; i < kOutputPixels; ++i) {
        std::fputc(output[i].x, file);
        std::fputc(output[i].y, file);
        std::fputc(output[i].z, file);
    }
    std::fclose(file);
}

}  // namespace

int main() {
    const char *remote = std::getenv("REMOTE_GPU_ADDRESS");
    std::printf("demo marker: batch-resize\n");
    std::printf("injected remote address: %s\n", remote == nullptr ? "(unset)" : remote);
    std::printf("batch: %d images, %dx%d -> %dx%d bilinear resize\n",
                kBatch, kInputWidth, kInputHeight, kOutputWidth, kOutputHeight);

    int device_count = 0;
    check_cuda(cudaGetDeviceCount(&device_count), "cudaGetDeviceCount");
    std::printf("device count: %d\n", device_count);
    if (device_count < 1) {
        return 1;
    }

    uchar4 *host_input = static_cast<uchar4 *>(
        std::malloc(sizeof(uchar4) * kBatch * kInputPixels));
    uchar4 *host_output = static_cast<uchar4 *>(
        std::malloc(sizeof(uchar4) * kBatch * kOutputPixels));
    unsigned long long *host_sums = static_cast<unsigned long long *>(
        std::calloc(kBatch * 4, sizeof(unsigned long long)));
    make_input(host_input);

    uchar4 *device_input = nullptr;
    uchar4 *device_output = nullptr;
    unsigned long long *device_sums = nullptr;
    check_cuda(cudaMalloc(&device_input,
                          sizeof(uchar4) * kBatch * kInputPixels),
               "cudaMalloc input");
    check_cuda(cudaMalloc(&device_output,
                          sizeof(uchar4) * kBatch * kOutputPixels),
               "cudaMalloc output");
    check_cuda(cudaMalloc(&device_sums,
                          sizeof(unsigned long long) * kBatch * 4),
               "cudaMalloc sums");

    auto started = std::chrono::steady_clock::now();
    check_cuda(cudaMemcpy(device_input, host_input,
                          sizeof(uchar4) * kBatch * kInputPixels,
                          cudaMemcpyHostToDevice), "cudaMemcpy input");
    check_cuda(cudaMemset(device_sums, 0,
                          sizeof(unsigned long long) * kBatch * 4),
               "cudaMemset sums");
    dim3 block(16, 16);
    dim3 grid((kOutputWidth + block.x - 1) / block.x,
              (kOutputHeight + block.y - 1) / block.y, kBatch);
    resize_batch<<<grid, block>>>(device_input, device_output);
    check_cuda(cudaGetLastError(), "resize_batch launch");
    int total = kBatch * kOutputPixels;
    accumulate_stats<<<(total + 255) / 256, 256>>>(device_output, device_sums);
    check_cuda(cudaGetLastError(), "accumulate_stats launch");
    check_cuda(cudaDeviceSynchronize(), "resize synchronize");
    check_cuda(cudaMemcpy(host_output, device_output,
                          sizeof(uchar4) * kBatch * kOutputPixels,
                          cudaMemcpyDeviceToHost), "cudaMemcpy output");
    check_cuda(cudaMemcpy(host_sums, device_sums,
                          sizeof(unsigned long long) * kBatch * 4,
                          cudaMemcpyDeviceToHost), "cudaMemcpy sums");
    auto finished = std::chrono::steady_clock::now();

    int failures = 0;
    for (int batch = 0; batch < kBatch; ++batch) {
        for (int y = 0; y < kOutputHeight; ++y) {
            for (int x = 0; x < kOutputWidth; ++x) {
                int index = batch * kOutputPixels + y * kOutputWidth + x;
                uchar4 expected = host_sample(host_input, batch, x, y);
                uchar4 actual = host_output[index];
                if (std::abs(static_cast<int>(actual.x)
                             - static_cast<int>(expected.x)) > 1
                    || std::abs(static_cast<int>(actual.y)
                                - static_cast<int>(expected.y)) > 1
                    || std::abs(static_cast<int>(actual.z)
                                - static_cast<int>(expected.z)) > 1) {
                    ++failures;
                }
            }
        }
    }
    unsigned long long expected_sum[kBatch][4] = {};
    for (int batch = 0; batch < kBatch; ++batch) {
        for (int i = 0; i < kOutputPixels; ++i) {
            uchar4 pixel = host_output[batch * kOutputPixels + i];
            expected_sum[batch][0] += pixel.x;
            expected_sum[batch][1] += pixel.y;
            expected_sum[batch][2] += pixel.z;
            expected_sum[batch][3] += 1;
        }
        for (int channel = 0; channel < 4; ++channel) {
            if (host_sums[batch * 4 + channel] != expected_sum[batch][channel]) {
                ++failures;
            }
        }
        double mean = static_cast<double>(expected_sum[batch][0]
            + expected_sum[batch][1] + expected_sum[batch][2])
            / static_cast<double>(3 * kOutputPixels);
        std::printf("image %d mean RGB: %.3f\n", batch, mean);
    }
    double elapsed_ms = std::chrono::duration<double, std::milli>(
        finished - started).count();
    const char *output = std::getenv("OUTPUT_PATH");
    write_ppm(output == nullptr ? "/tmp/batch-resize.ppm" : output, host_output);
    std::printf("resize wall time ms: %.3f\n", elapsed_ms);
    std::printf("verification failures: %d\n", failures);
    std::printf("sample image0 RGB: %u %u %u\n",
                host_output[0].x, host_output[0].y, host_output[0].z);

    cudaFree(device_sums);
    cudaFree(device_output);
    cudaFree(device_input);
    std::free(host_sums);
    std::free(host_output);
    std::free(host_input);
    return failures == 0 ? 0 : 8;
}
