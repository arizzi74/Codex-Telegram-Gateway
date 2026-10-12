#include "protocol.h"
#include "segments.h"
#include <iostream>
#include <limits>
#include <sstream>

int main() {
    try {
        const auto r = protocol::parse("{\"text\":\"Ciao, \\u00e8 \\uD83D\\uDE00!\\n\\\"prova\\\"\",\"id\":\"one\"}");
        if (r.id != "one" || r.text != "Ciao, è 😀!\n\"prova\"") return 1;
        for (const auto &bad : std::vector<std::string>{"{}", "[]", "{\"id\":\"a\",\"text\":\" \"}",
                "{\"id\":\"a\",\"text\":\"\\u2003\\u00a0\"}", "{\"id\":\"a\",\"text\":\"\\u0000\"}",
                "{\"id\":\"a\",\"text\":\"x\",\"id\":\"b\"}", "{\"id\":\"a\",\"text\":[]}",
                "{\"id\":\"a\",\"text\":{\"key\":\"x\"}}", "{\"id\":\"a\",\"text\":\"x\",\"path\":\"x\"}",
                "{\"id\":\"a\",\"text\":\"x\"}{}", "{\"id\":\"a\",\"text\":\"\\uD800\"}",
                std::string("{\"id\":\"a\",\"text\":\"") + char(0xff) + "\"}",
                nlohmann::json({{"id", std::string(33, 'i')}, {"text", "x"}}).dump(),
                nlohmann::json({{"id", "a"}, {"text", std::string(protocol::max_text + 1, 'x')}}).dump()}) {
            bool rejected = false;
            try { protocol::parse(bad); } catch (const std::exception &) { rejected = true; }
            if (!rejected) throw std::runtime_error("accepted invalid request");
        }
        if (protocol::parse(nlohmann::json({{"id", "max"}, {"text", std::string(protocol::max_text, 'x')}}).dump()).text.size() != protocol::max_text) return 1;
        std::string line;
        std::istringstream good("{}\n");
        if (!protocol::header(good, line) || line != "{}" || protocol::header(good, line)) return 1;
        for (const auto &bad : {std::string("{}"), std::string(protocol::max_header, 'x') + "\n"}) {
            bool rejected = false;
            std::istringstream input(bad);
            try { protocol::header(input, line); } catch (const std::exception &) { rejected = true; }
            if (!rejected) return 1;
        }
        std::ostringstream output;
        protocol::generated(output, "pcm", {-2.0f, 0.5f, 2.0f});
        const auto encoded = output.str();
        const auto offset = encoded.find('\n') + 1;
        const unsigned char expected[] = {0, 0, 128, 191, 0, 0, 0, 63, 0, 0, 128, 63};
        if (encoded.size() != offset + sizeof(expected) || std::memcmp(encoded.data() + offset, expected, sizeof(expected)) != 0) return 1;
        for (const float bad : {std::numeric_limits<float>::infinity(), std::numeric_limits<float>::quiet_NaN()}) {
            std::ostringstream untouched;
            bool rejected = false;
            try { protocol::generated(untouched, "nan", {bad}); } catch (const std::exception &) { rejected = true; }
            if (!rejected || !untouched.str().empty()) return 1;
        }
        const std::string long_text = std::string(1000, 'x') + " è 😀. " + std::string(1200, 'y');
        const auto parts = speech::segments(long_text, [](const std::string &s) { return static_cast<int>(s.size()); });
        std::string joined;
        for (const auto &part : parts) {
            if (part.size() > speech::max_segment_tokens) return 1;
            if (nlohmann::json::parse(nlohmann::json(part).dump()).get<std::string>() != part) return 1;
            joined += part;
        }
        if (joined != long_text || parts.size() < 2) return 1;
        return 0;
    } catch (const std::exception &error) { std::cerr << error.what() << '\n'; return 1; }
}
