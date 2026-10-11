#include "protocol.h"
#include "whisper.h"
#include <cmath>
#include <cstdio>
#include <cstring>
#include <iostream>
#include <memory>
#include <vector>

namespace {
void log_stderr(enum ggml_log_level, const char *text, void *) { std::fputs(text, stderr); }
void reply(const protocol::request &r, const std::string &text, const std::string &language, const std::string &error) {
    auto encoded = protocol::quote(text);
    auto failure = error;
    if (encoded.size() > 120 * 1024) { encoded = "\"\""; failure = "transcript exceeds encoded size limit"; }
    std::cout << "{\"id\":" << protocol::quote(r.id) << ",\"text\":" << encoded
              << ",\"language\":" << protocol::quote(language) << ",\"error\":" << protocol::quote(failure) << "}\n" << std::flush;
}
bool header(std::string &line) {
    line.clear(); char c;
    while (std::cin.get(c)) {
        if (c == '\n') return true;
        if (line.size() == protocol::max_header) throw std::runtime_error("request header too large");
        line += c;
    }
    if (!line.empty()) throw std::runtime_error("truncated request header");
    return false;
}
}

int main(int argc, char **argv) {
    try {
        std::string model; int threads = 2;
        if (argc == 2 && std::string(argv[1]) == "version") { std::cout << CODEX_WHISPER_VERSION << '\n'; return 0; }
        for (int i = 1; i < argc; ++i) {
            const std::string arg = argv[i];
            if (arg == "--model" && i + 1 < argc) model = argv[++i];
            else if (arg == "--threads" && i + 1 < argc) {
                const std::string value = argv[++i]; size_t used = 0;
                threads = std::stoi(value, &used);
                if (used != value.size() || threads < 1 || threads > 32) throw std::runtime_error("threads must be between 1 and 32");
            } else throw std::runtime_error("usage: codex-whisper --model PATH [--threads N]");
        }
        if (model.empty()) throw std::runtime_error("model path is required");
        whisper_log_set(log_stderr, nullptr);
        auto params = whisper_context_default_params(); params.use_gpu = false; params.flash_attn = false;
        std::unique_ptr<whisper_context, decltype(&whisper_free)> context(whisper_init_from_file_with_params(model.c_str(), params), whisper_free);
        if (!context) throw std::runtime_error("could not load Whisper model");
        if (!whisper_is_multilingual(context.get())) throw std::runtime_error("a multilingual Whisper model is required");
        std::cout << "{\"type\":\"ready\",\"protocol\":1}\n" << std::flush;
        std::string line;
        while (header(line)) {
            const auto request = protocol::parser(line).parse();
            std::vector<float> pcm(request.samples);
            unsigned char bytes[4];
            for (float &sample : pcm) {
                if (!std::cin.read(reinterpret_cast<char *>(bytes), 4)) throw std::runtime_error("truncated PCM input");
                const uint32_t word = uint32_t(bytes[0]) | (uint32_t(bytes[1]) << 8) | (uint32_t(bytes[2]) << 16) | (uint32_t(bytes[3]) << 24);
                std::memcpy(&sample, &word, sizeof(sample));
                if (!std::isfinite(sample) || sample < -1.0f || sample > 1.0f) throw std::runtime_error("invalid PCM sample");
            }
            if (request.language != "auto" && whisper_lang_id(request.language.c_str()) < 0) {
                reply(request, "", "", "unsupported language"); continue;
            }
            auto decode = whisper_full_default_params(WHISPER_SAMPLING_GREEDY);
            decode.n_threads = threads; decode.no_context = true; decode.n_max_text_ctx = 0;
            decode.no_timestamps = true; decode.translate = false;
            decode.print_special = false; decode.print_progress = false; decode.print_realtime = false; decode.print_timestamps = false;
            decode.language = request.language.c_str();
            // `detect_language=true` means detect-only in whisper.cpp; auto is
            // already detected during a normal full transcription.
            decode.detect_language = false;
            if (whisper_full(context.get(), decode, pcm.data(), static_cast<int>(pcm.size())) != 0) {
                reply(request, "", "", "Whisper transcription failed"); continue;
            }
            std::string text;
            for (int i = 0; i < whisper_full_n_segments(context.get()); ++i) {
                const char *segment = whisper_full_get_segment_text(context.get(), i);
                if (segment) text += segment;
                if (text.size() > protocol::max_text) break;
            }
            if (text.size() > protocol::max_text) { reply(request, "", "", "transcript exceeds size limit"); continue; }
            const char *language = whisper_lang_str(whisper_full_lang_id(context.get()));
            reply(request, text, language ? language : "", "");
        }
        return 0;
    } catch (const std::exception &error) {
        // Framing errors terminate the stream; continuing could interpret PCM
        // as another request. The gateway discards and restarts this process.
        std::fprintf(stderr, "codex-whisper: %s\n", error.what()); return 1;
    }
}
