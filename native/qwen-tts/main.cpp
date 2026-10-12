#include "protocol.h"
#include "segments.h"
#include "stock-voice.h"
#include "llama.h"
#include "mtmd.h"
#include "mtmd-helper.h"
#include <chrono>
#include <cstdio>
#include <filesystem>
#include <iostream>
#include <memory>
#include <random>

namespace {
void diagnostics(enum ggml_log_level, const char *message, void *) { std::fputs(message, stderr); }
struct deadline {
    std::chrono::steady_clock::time_point end = std::chrono::steady_clock::time_point::max();
    static bool expired(void *data) { return std::chrono::steady_clock::now() >= static_cast<deadline *>(data)->end; }
};

class engine {
    std::unique_ptr<llama_model, decltype(&llama_model_free)> model{nullptr, llama_model_free};
    std::unique_ptr<llama_context, decltype(&llama_free)> context{nullptr, llama_free};
    mtmd::context_ptr audio;
    mtmd::bitmap_ptr speaker;
    std::unique_ptr<mtmd_helper::gen_audio> generator;
    const llama_vocab *vocab = nullptr;
    std::string language;
    deadline limit{};
    uint32_t seed = std::random_device{}();

    int token_count(const std::string &text) const {
        const int count = llama_tokenize(vocab, text.data(), static_cast<int32_t>(text.size()), nullptr, 0, false, true);
        if (count == 0) throw std::runtime_error("empty tokenized text");
        return count < 0 ? -count : count;
    }

    void reset() {
        generator->reset();
        llama_memory_clear(llama_get_memory(context.get()), true);
    }

    void segment(const std::string &text, std::vector<float> &samples) {
        reset();
        auto settings = llama_sampler_chain_default_params();
        settings.no_perf = true;
        std::unique_ptr<llama_sampler, decltype(&llama_sampler_free)> sampler(llama_sampler_chain_init(settings), llama_sampler_free);
        llama_sampler_chain_add(sampler.get(), llama_sampler_init_penalties(llama_vocab_n_tokens(vocab), 8192, 1.05f, 0.0f, 0.0f));
        llama_sampler_chain_add(sampler.get(), llama_sampler_init_temp(0.9f));
        llama_sampler_chain_add(sampler.get(), llama_sampler_init_top_k(50));
        llama_sampler_chain_add(sampler.get(), llama_sampler_init_top_p(1.0f, 1));
        // A new seed resets mtmd's code predictor RNG between segments/requests.
        ++seed;
        if (seed == UINT32_MAX) ++seed;
        llama_sampler_chain_add(sampler.get(), llama_sampler_init_dist(seed));
        mtmd_helper_gen_audio_inp input{};
        input.seq_id = 0;
        input.prompt = text.data();
        input.prompt_len = text.size();
        input.speaker_ref = speaker.get();
        input.lang = language.c_str();
        input.top_k = 50;
        input.top_p = 1.0f;
        input.seed = seed;
        input.out_type = MTMD_HELPER_GEN_AUDIO_OUTTYPE_PCM;
        if (generator->set_input(&input) != 0) throw std::runtime_error("speech input failed");
        for (;;) {
            if (deadline::expired(&limit)) throw std::runtime_error("speech deadline exceeded");
            const int pending = generator->step_prompt(256);
            if (pending < 0) throw std::runtime_error("speech prompt failed");
            if (pending == 0) break;
        }
        const int max_frames = std::min(1800, 96 + token_count(text) * 12);
        const float *hidden = llama_get_embeddings_ith(context.get(), -1);
        if (!hidden) throw std::runtime_error("missing speech hidden state");
        bool stopped = false;
        for (int frame = 0; frame < max_frames; ++frame) {
            if (deadline::expired(&limit)) throw std::runtime_error("speech deadline exceeded");
            const llama_token token = llama_sampler_sample(sampler.get(), context.get(), -1);
            const float *next = nullptr;
            if (generator->step_gen(token, hidden, &next, &stopped) != 0) throw std::runtime_error("speech generation failed");
            if (stopped) break;
            if (!next) throw std::runtime_error("speech stopped without end token");
            hidden = next;
        }
        if (!stopped) throw std::runtime_error("speech did not finish within its frame bound");
        int32_t rate = 0;
        const char *data = nullptr;
        size_t bytes = 0;
        int64_t count = 0;
        if (generator->get_output(&rate, &data, &bytes, &count) != 0 || rate != protocol::sample_rate ||
            count <= 0 || static_cast<uint64_t>(count) > protocol::max_samples - samples.size() ||
            bytes != static_cast<size_t>(count) * sizeof(float) || !data) throw std::runtime_error("invalid speech output");
        const auto *pcm = reinterpret_cast<const float *>(data);
        for (int64_t i = 0; i < count; ++i) if (!std::isfinite(pcm[i])) throw std::runtime_error("nonfinite speech output");
        samples.insert(samples.end(), pcm, pcm + count);
    }

public:
    engine(const std::filesystem::path &dir, const std::string &lang, int threads) : language(lang) {
        auto mp = llama_model_default_params();
        mp.n_gpu_layers = 0;
        mp.check_tensors = true;
        model.reset(llama_model_load_from_file((dir / "Qwen3-TTS-12Hz-1.7B-Base-Q4_K_M.gguf").c_str(), mp));
        if (!model) throw std::runtime_error("speech model loading failed");
        vocab = llama_model_get_vocab(model.get());
        auto cp = llama_context_default_params();
        cp.n_ctx = 4096;
        cp.n_batch = cp.n_ubatch = 256;
        cp.n_seq_max = 1;
        cp.n_threads = cp.n_threads_batch = threads;
        cp.embeddings = true;
        cp.offload_kqv = cp.op_offload = false;
        cp.no_perf = true;
        cp.abort_callback = deadline::expired;
        cp.abort_callback_data = &limit;
        context.reset(llama_init_from_model(model.get(), cp));
        if (!context) throw std::runtime_error("speech context loading failed");
        auto ap = mtmd_context_params_default();
        ap.use_gpu = false;
        ap.n_threads = threads;
        ap.print_timings = false;
        audio.reset(mtmd_init_from_file((dir / "mmproj-Qwen3-TTS-12Hz-1.7B-Base-Q8_0.gguf").c_str(), model.get(), ap));
        if (!audio || mtmd_gen_audio_get_info(audio.get()).type != MTMD_GEN_AUDIO_TYPE_QWEN3TTS ||
            mtmd_gen_audio_get_info(audio.get()).sample_rate != protocol::sample_rate) throw std::runtime_error("speech audio model loading failed");
        const auto reference = mtmd_helper_bitmap_init_from_buf(audio.get(), stock_voice, sizeof(stock_voice), false, mtmd_helper_init_opt_default());
        speaker.reset(reference.bitmap);
        if (!speaker) throw std::runtime_error("stock voice loading failed");
        generator = std::make_unique<mtmd_helper::gen_audio>(context.get(), audio.get());
    }

    std::vector<float> synthesize(const std::string &text) {
        limit.end = std::chrono::steady_clock::now() + std::chrono::minutes(9);
        std::vector<float> samples;
        try {
            for (const auto &part : speech::segments(text, [this](const std::string &part) { return token_count(part); })) segment(part, samples);
            reset();
            return samples;
        } catch (...) {
            std::fill(samples.begin(), samples.end(), 0.0f);
            reset();
            throw;
        }
    }
};
}

int main(int argc, char **argv) {
    try {
        if (argc == 2 && std::string(argv[1]) == "version") { std::cout << CODEX_QWEN_TTS_VERSION << '\n'; return 0; }
        std::filesystem::path dir;
        std::string language;
        int threads = 0;
        unsigned flags = 0;
        for (int i = 1; i < argc; ++i) {
            const std::string arg = argv[i];
            const unsigned flag = arg == "--dir" ? 1 : arg == "--language" ? 2 : arg == "--threads" ? 4 : 0;
            if (!flag || (flags & flag) || i + 1 == argc) throw std::runtime_error("invalid helper arguments");
            flags |= flag;
            const std::string value = argv[++i];
            if (flag == 1) dir = value;
            else if (flag == 2) language = value;
            else {
                size_t used = 0;
                threads = std::stoi(value, &used);
                if (used != value.size() || threads < 1 || threads > 32) throw std::runtime_error("invalid CPU thread count");
            }
        }
        if (flags != 7 || !dir.is_absolute() || (language != "italian" && language != "english")) throw std::runtime_error("invalid helper arguments");
        llama_log_set(diagnostics, nullptr);
        mtmd_helper_log_set(diagnostics, nullptr);
        llama_backend_init();
        {
            engine resident(dir, language, threads);
            protocol::json(std::cout, {{"type", "ready"}, {"protocol", 1}, {"sample_rate", protocol::sample_rate}});
            std::string line;
            while (protocol::header(std::cin, line)) {
                const auto request = protocol::parse(line);
                std::vector<float> samples;
                try { samples = resident.synthesize(request.text); }
                catch (const std::exception &) {
                    protocol::json(std::cout, {{"id", request.id}, {"error", "synthesis_failed"}});
                    continue;
                }
                protocol::generated(std::cout, request.id, samples);
                std::fill(samples.begin(), samples.end(), 0.0f);
                std::fill(line.begin(), line.end(), '\0');
            }
        }
        llama_backend_free();
        return 0;
    } catch (const std::exception &) {
        // Never include request text or model paths in diagnostics.
        std::fputs("codex-qwen-tts: helper failed\n", stderr);
        return 1;
    }
}
