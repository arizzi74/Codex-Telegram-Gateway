#include "protocol.h"
#include "sherpa-onnx/c-api/c-api.h"
#include <algorithm>
#include <chrono>
#include <cmath>
#include <cstdio>
#include <cstring>
#include <iostream>
#include <memory>
#include <vector>

namespace {
struct generation_bound {
    int64_t samples = 0;
    bool stopped = false;
    std::chrono::steady_clock::time_point deadline = std::chrono::steady_clock::now() + std::chrono::minutes(10);
};
int32_t progress(const float *, int32_t n, float, void *arg) {
    auto &bound = *static_cast<generation_bound *>(arg);
    bound.samples += std::max(n, int32_t(0));
    if (n < 0 || bound.samples > 22050 * 600 || std::chrono::steady_clock::now() > bound.deadline) bound.stopped = true;
    return bound.stopped ? 0 : 1;
}
bool finite(const float *samples, int n) {
    if (!samples || n <= 0) return false;
    for (int i = 0; i < n; ++i) if (!std::isfinite(samples[i])) return false;
    return true;
}
void failure(const std::string &id) {
    std::cout << "{\"id\":" << protocol::quote(id) << ",\"error\":\"synthesis_failed\"}\n" << std::flush;
    if (!std::cout) throw std::runtime_error("output pipe failed");
}
void reply(const std::string &id, const float *samples, int n) {
    std::cout << "{\"id\":" << protocol::quote(id) << ",\"sample_count\":" << n << ",\"sample_rate\":24000}\n";
    static_assert(sizeof(float) == sizeof(uint32_t), "protocol requires IEEE float32");
    unsigned char bytes[32 * 1024];
    for (int offset = 0; offset < n;) {
        const int count = std::min(n - offset, static_cast<int>(sizeof(bytes) / 4));
        for (int i = 0; i < count; ++i) {
            const float sample = std::max(-1.0f, std::min(1.0f, samples[offset + i])); uint32_t word;
            std::memcpy(&word, &sample, 4);
            bytes[i*4] = word; bytes[i*4+1] = word >> 8; bytes[i*4+2] = word >> 16; bytes[i*4+3] = word >> 24;
        }
        std::cout.write(reinterpret_cast<const char *>(bytes), count * 4); offset += count;
        if (!std::cout) throw std::runtime_error("output pipe failed");
    }
    std::memset(bytes, 0, sizeof(bytes)); std::cout.flush();
    if (!std::cout) throw std::runtime_error("output pipe failed");
}
}

int main(int argc, char **argv) {
    try {
        if (argc == 2 && std::string(argv[1]) == "version") { std::cout << CODEX_PIPER_VERSION << '\n'; return 0; }
        std::string dir, language; int threads = 0; unsigned flags = 0;
        for (int i = 1; i < argc; ++i) {
            const std::string arg = argv[i];
            if (arg == "--dir" && i + 1 < argc && !(flags & 1)) { dir = argv[++i]; flags |= 1; }
            else if (arg == "--language" && i + 1 < argc && !(flags & 2)) { language = argv[++i]; flags |= 2; }
            else if (arg == "--threads" && i + 1 < argc && !(flags & 4)) {
                const std::string value = argv[++i]; size_t used = 0;
                if (value.empty() || value.find_first_not_of("0123456789") != std::string::npos) throw std::runtime_error("invalid CPU threads");
                threads = std::stoi(value, &used); flags |= 4;
                if (used != value.size() || threads < 1 || threads > 32) throw std::runtime_error("CPU threads must be between 1 and 32");
            } else throw std::runtime_error("usage: codex-piper --dir ENGINE_DIR --language italian|english --threads N");
        }
        if (flags != 7 || dir.empty() || dir[0] != '/' || (language != "italian" && language != "english")) throw std::runtime_error("invalid helper arguments");
        // Miro is an Italian voice. English mode permits English technical text
        // through that same voice; it does not switch to an English model.
        const auto base = dir + "/vits-piper-it_IT-miro-high/";
        const auto model = base + "it_IT-miro-high.onnx", tokens = base + "tokens.txt", data = base + "espeak-ng-data";
        SherpaOnnxOfflineTtsConfig config{};
        config.model.vits.model = model.c_str(); config.model.vits.tokens = tokens.c_str(); config.model.vits.data_dir = data.c_str();
        config.model.vits.noise_scale = 0.667f; config.model.vits.noise_scale_w = 0.8f; config.model.vits.length_scale = 1.0f;
        config.model.num_threads = threads; config.model.provider = "cpu"; config.max_num_sentences = 1; config.silence_scale = 0.2f;
        std::unique_ptr<const SherpaOnnxOfflineTts, decltype(&SherpaOnnxDestroyOfflineTts)> tts(SherpaOnnxCreateOfflineTts(&config), SherpaOnnxDestroyOfflineTts);
        if (!tts || SherpaOnnxOfflineTtsSampleRate(tts.get()) != 22050) throw std::runtime_error("could not load Miro 22050 Hz model");
        // Kaldi's windowed-sinc resampler includes anti-imaging filtering.
        // Flush and reset between requests; no waveform state crosses replies.
        std::unique_ptr<const SherpaOnnxLinearResampler, decltype(&SherpaOnnxDestroyLinearResampler)> resampler(SherpaOnnxCreateLinearResampler(22050, protocol::sample_rate, 0, 0), SherpaOnnxDestroyLinearResampler);
        if (!resampler) throw std::runtime_error("could not create sample rate converter");
        std::cout << "{\"type\":\"ready\",\"protocol\":1,\"sample_rate\":24000}\n" << std::flush;
        if (!std::cout) throw std::runtime_error("output pipe failed");
        std::string line;
        while (protocol::header(std::cin, line)) {
            auto request = protocol::parser(line).parse(); protocol::wipe(line);
            SherpaOnnxGenerationConfig generation{}; generation.speed = 1.0f; generation.silence_scale = 0.2f;
            generation_bound bound;
            std::unique_ptr<const SherpaOnnxGeneratedAudio, decltype(&SherpaOnnxDestroyOfflineTtsGeneratedAudio)> audio(SherpaOnnxOfflineTtsGenerateWithConfig(tts.get(), request.text.c_str(), &generation, progress, &bound), SherpaOnnxDestroyOfflineTtsGeneratedAudio);
            protocol::wipe(request.text);
            if (bound.stopped || !audio || audio->sample_rate != 22050 || audio->n > 22050 * 600 || !finite(audio->samples, audio->n)) { failure(request.id); continue; }
            SherpaOnnxLinearResamplerReset(resampler.get());
            std::unique_ptr<const SherpaOnnxResampleOut, decltype(&SherpaOnnxLinearResamplerResampleFree)> out(SherpaOnnxLinearResamplerResample(resampler.get(), audio->samples, audio->n, 1), SherpaOnnxLinearResamplerResampleFree);
            if (!out || out->n > protocol::max_samples || !finite(out->samples, out->n)) { failure(request.id); continue; }
            reply(request.id, out->samples, out->n);
        }
        return 0;
    } catch (const std::exception &) {
        // Native diagnostics must never include request text. Framing failures
        // terminate the stream and let the gateway replace this process.
        std::fputs("codex-piper: helper initialization, framing or pipe failed\n", stderr); return 1;
    }
}
