#include "sherpa-onnx/c-api/c-api.h"
#include <cmath>
#include <memory>
#include <vector>

int main() {
    std::unique_ptr<const SherpaOnnxLinearResampler, decltype(&SherpaOnnxDestroyLinearResampler)> converter(SherpaOnnxCreateLinearResampler(22050, 24000, 0, 0), SherpaOnnxDestroyLinearResampler);
    if (!converter) return 1;
    std::vector<float> source(22050);
    const double pi = std::acos(-1.0);
    for (size_t i = 0; i < source.size(); ++i) source[i] = static_cast<float>(0.5 * std::sin(2 * pi * 1000 * i / 22050));
    std::vector<float> first;
    for (int request = 0; request < 2; ++request) {
        SherpaOnnxLinearResamplerReset(converter.get());
        std::unique_ptr<const SherpaOnnxResampleOut, decltype(&SherpaOnnxLinearResamplerResampleFree)> output(SherpaOnnxLinearResamplerResample(converter.get(), source.data(), source.size(), 1), SherpaOnnxLinearResamplerResampleFree);
        if (!output || output->n != 24000 || !output->samples) return 2;
        double error = 0;
        for (int i = 32; i < output->n - 32; ++i) {
            if (!std::isfinite(output->samples[i])) return 3;
            const double residual = output->samples[i] - 0.5 * std::sin(2 * pi * 1000 * i / 24000);
            error += residual * residual;
        }
        if (std::sqrt(error / (output->n - 64)) > 0.005) return 4;
        if (request == 0) first.assign(output->samples, output->samples + output->n);
        else for (int i = 0; i < output->n; ++i) if (first[i] != output->samples[i]) return 5;
    }
    return 0;
}
