#pragma once
#include "protocol.h"
#include <algorithm>
#include <functional>
#include <stdexcept>
#include <string>
#include <vector>

namespace speech {
constexpr int max_segment_tokens = 256;
inline std::vector<std::string> segments(const std::string &text, const std::function<int(const std::string &)> &count) {
    std::vector<std::string> result;
    size_t start = 0;
    while (start < text.size()) {
        size_t end = std::min(text.size(), start + 1024);
        while (end < text.size() && (static_cast<unsigned char>(text[end]) & 0xc0) == 0x80) --end;
        while (end > start && count(text.substr(start, end - start)) > max_segment_tokens) {
            end = start + (end - start) / 2;
            while (end > start && (static_cast<unsigned char>(text[end]) & 0xc0) == 0x80) --end;
        }
        if (end == start) throw std::runtime_error("text cannot fit a speech segment");
        if (end < text.size()) {
            size_t sentence = 0, space = 0;
            for (size_t i = start; i < end; ++i) {
                const char c = text[i];
                if (c == ' ' || c == '\t' || c == '\n' || c == '\r') space = i + 1;
                if (c == '.' || c == '!' || c == '?' || c == '\n' || c == ';') sentence = i + 1;
            }
            if (sentence > start + (end - start) / 3) end = sentence;
            else if (space > start + (end - start) / 3) end = space;
        }
        const auto part = text.substr(start, end - start);
        if (!protocol::whitespace_only(part)) result.push_back(part);
        start = end;
    }
    if (result.empty()) throw std::runtime_error("empty speech request");
    return result;
}
}
