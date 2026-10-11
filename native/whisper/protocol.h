#pragma once
#include <cctype>
#include <cstdint>
#include <stdexcept>
#include <string>

namespace protocol {
constexpr size_t max_header = 4096;
constexpr uint32_t max_samples = 16000 * 600;
constexpr size_t max_text = 64 * 1024;
struct request { std::string id, language; uint32_t samples = 0; };

// A deliberately narrow JSON object: protocol identifiers and language codes
// are ASCII, and exactly these three fields are accepted in any order.
class parser {
    const std::string &s;
    size_t p = 0;
    void ws() { while (p < s.size() && (s[p] == ' ' || s[p] == '\t' || s[p] == '\r')) ++p; }
    void token(char c) { ws(); if (p == s.size() || s[p++] != c) throw std::runtime_error("invalid request header"); }
    std::string string() {
        token('"'); std::string out;
        while (p < s.size() && s[p] != '"') {
            const unsigned char c = s[p++];
            if (c < 32 || c > 126 || c == '\\') throw std::runtime_error("invalid request string");
            out += static_cast<char>(c);
        }
        token('"'); return out;
    }
public:
    explicit parser(const std::string &input) : s(input) {}
    request parse() {
        if (s.size() > max_header) throw std::runtime_error("request header too large");
        request r; unsigned fields = 0; token('{');
        for (;;) {
            const auto key = string(); token(':');
            if (key == "id") {
                if (fields & 1) throw std::runtime_error("duplicate request field");
                fields |= 1; r.id = string();
                if (r.id.empty() || r.id.size() > 128) throw std::runtime_error("invalid request id");
                for (unsigned char c : r.id) if (!std::isalnum(c) && c != '-' && c != '_' && c != '.') throw std::runtime_error("invalid request id");
            } else if (key == "language") {
                if (fields & 2) throw std::runtime_error("duplicate request field");
                fields |= 2; r.language = string();
                if (r.language.empty() || r.language.size() > 16) throw std::runtime_error("invalid language");
                for (unsigned char c : r.language) if (!(c >= 'a' && c <= 'z') && c != '-') throw std::runtime_error("invalid language");
            } else if (key == "sample_count") {
                if (fields & 4) throw std::runtime_error("duplicate request field");
                fields |= 4; ws(); const auto start = p; uint64_t n = 0;
                while (p < s.size() && s[p] >= '0' && s[p] <= '9') {
                    n = n * 10 + static_cast<unsigned>(s[p++] - '0');
                    if (n > max_samples) throw std::runtime_error("audio exceeds sample limit");
                }
                if (p == start || n == 0 || (p > start + 1 && s[start] == '0')) throw std::runtime_error("invalid sample_count");
                r.samples = static_cast<uint32_t>(n);
            } else throw std::runtime_error("unknown request field");
            ws(); if (p < s.size() && s[p] == ',') { ++p; continue; }
            token('}'); ws();
            if (p != s.size() || fields != 7) throw std::runtime_error("invalid request header");
            return r;
        }
    }
};

inline std::string quote(const std::string &s) {
    static const char hex[] = "0123456789abcdef";
    std::string out = "\"";
    for (unsigned char c : s) {
        if (c == '"' || c == '\\') { out += '\\'; out += static_cast<char>(c); }
        else if (c < 32) { out += "\\u00"; out += hex[c >> 4]; out += hex[c & 15]; }
        else out += static_cast<char>(c);
    }
    return out + '"';
}
}
