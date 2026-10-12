#pragma once
#include <nlohmann/json.hpp>
#include <algorithm>
#include <array>
#include <cmath>
#include <cstdint>
#include <cstring>
#include <istream>
#include <ostream>
#include <stdexcept>
#include <string>
#include <vector>

namespace protocol {
constexpr size_t max_header = 64 * 1024;
constexpr size_t max_text = 32 * 1024;
constexpr size_t sample_rate = 24000;
constexpr size_t max_samples = sample_rate * 600;
struct request { std::string id, text; };

inline bool header(std::istream &input, std::string &line) {
    line.clear(); char c;
    while (input.get(c)) {
        if (c == '\n') return true;
        if (line.size() >= max_header - 1) throw std::runtime_error("request header too large");
        line += c;
    }
    if (!input.eof()) throw std::runtime_error("request read failed");
    if (!line.empty()) throw std::runtime_error("truncated request header");
    return false;
}

inline bool whitespace_only(const std::string &text) {
    // JSON parsing has already checked UTF-8. Match Go's Unicode whitespace.
    for (size_t i = 0; i < text.size();) {
        uint32_t cp = static_cast<unsigned char>(text[i++]);
        int continuation = 0;
        if (cp >= 0xf0) { cp &= 7; continuation = 3; }
        else if (cp >= 0xe0) { cp &= 15; continuation = 2; }
        else if (cp >= 0xc0) { cp &= 31; continuation = 1; }
        for (int j = 0; j < continuation; ++j) cp = (cp << 6) | (static_cast<unsigned char>(text[i++]) & 63);
        if (!(cp == 0x20 || (cp >= 9 && cp <= 13) || cp == 0x85 || cp == 0xa0 || cp == 0x1680 ||
              (cp >= 0x2000 && cp <= 0x200a) || cp == 0x2028 || cp == 0x2029 || cp == 0x202f || cp == 0x205f || cp == 0x3000)) return false;
    }
    return true;
}

inline request parse(const std::string &line) {
    if (line.size() + 1 > max_header) throw std::runtime_error("request header too large");
    unsigned fields = 0;
    auto check = [&fields](int depth, nlohmann::json::parse_event_t event, nlohmann::json &value) {
        if (depth > 1) throw std::runtime_error("nested request field");
        if (event == nlohmann::json::parse_event_t::key) {
            const auto key = value.get<std::string>();
            const unsigned bit = key == "id" ? 1 : key == "text" ? 2 : 0;
            if (!bit || (fields & bit)) throw std::runtime_error("unknown or duplicate request field");
            fields |= bit;
        }
        return true;
    };
    const auto value = nlohmann::json::parse(line, check);
    if (!value.is_object() || fields != 3 || !value["id"].is_string() || !value["text"].is_string()) throw std::runtime_error("invalid request");
    request result{value["id"].get<std::string>(), value["text"].get<std::string>()};
    if (result.id.empty() || result.id.size() > 32 || result.id.find('\0') != std::string::npos ||
        result.text.size() > max_text || result.text.find('\0') != std::string::npos || whitespace_only(result.text)) throw std::runtime_error("invalid request");
    return result;
}

inline void json(std::ostream &output, const nlohmann::json &value) {
    const auto line = value.dump();
    if (line.size() + 1 > max_header) throw std::runtime_error("response header too large");
    output << line << '\n';
    output.flush();
    if (!output) throw std::runtime_error("response write failed");
}

inline void generated(std::ostream &output, const std::string &id, const std::vector<float> &samples) {
    if (samples.empty() || samples.size() > max_samples) throw std::runtime_error("invalid audio length");
    for (float sample : samples) if (!std::isfinite(sample)) throw std::runtime_error("nonfinite audio");
    json(output, {{"id", id}, {"sample_count", samples.size()}, {"sample_rate", sample_rate}});
    std::array<unsigned char, 32 * 1024> chunk{};
    for (size_t offset = 0; offset < samples.size();) {
        const size_t count = std::min(chunk.size() / 4, samples.size() - offset);
        for (size_t i = 0; i < count; ++i) {
            const float sample = std::max(-1.0f, std::min(1.0f, samples[offset + i]));
            uint32_t word;
            static_assert(sizeof(float) == 4, "protocol requires float32");
            std::memcpy(&word, &sample, 4);
            for (unsigned j = 0; j < 4; ++j) chunk[i * 4 + j] = static_cast<unsigned char>(word >> (j * 8));
        }
        output.write(reinterpret_cast<const char *>(chunk.data()), static_cast<std::streamsize>(count * 4));
        if (!output) throw std::runtime_error("PCM write failed");
        offset += count;
    }
    output.flush();
    if (!output) throw std::runtime_error("PCM flush failed");
}
}
