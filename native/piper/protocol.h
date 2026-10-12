#pragma once
#include <cstdint>
#include <istream>
#include <stdexcept>
#include <string>

namespace protocol {
constexpr size_t max_header = 64 * 1024;
constexpr size_t max_text = 32 * 1024;
constexpr int sample_rate = 24000;
constexpr int max_samples = sample_rate * 600;
struct request { std::string id, text; };

inline void wipe(std::string &s) {
    volatile char *p = s.empty() ? nullptr : &s[0];
    for (size_t i = 0; i < s.size(); ++i) p[i] = 0;
    s.clear();
}

// Decode scalar values strictly: reject overlong sequences, surrogate code
// points, truncated UTF-8 and values outside the Unicode range.
inline uint32_t scalar(const std::string &s, size_t &p) {
    if (p == s.size()) throw std::runtime_error("invalid UTF-8");
    uint32_t c = static_cast<unsigned char>(s[p++]);
    if (c < 0x80) return c;
    int n; uint32_t minimum;
    if (c >= 0xc2 && c <= 0xdf) { n = 1; c &= 0x1f; minimum = 0x80; }
    else if (c >= 0xe0 && c <= 0xef) { n = 2; c &= 0x0f; minimum = 0x800; }
    else if (c >= 0xf0 && c <= 0xf4) { n = 3; c &= 7; minimum = 0x10000; }
    else throw std::runtime_error("invalid UTF-8");
    while (n--) {
        if (p == s.size()) throw std::runtime_error("invalid UTF-8");
        const auto b = static_cast<unsigned char>(s[p++]);
        if ((b & 0xc0) != 0x80) throw std::runtime_error("invalid UTF-8");
        c = (c << 6) | (b & 0x3f);
    }
    if (c < minimum || c > 0x10ffff || (c >= 0xd800 && c <= 0xdfff)) throw std::runtime_error("invalid UTF-8");
    return c;
}
inline bool space(uint32_t c) {
    return (c >= 9 && c <= 13) || c == 32 || c == 0x85 || c == 0xa0 || c == 0x1680 ||
           (c >= 0x2000 && c <= 0x200a) || c == 0x2028 || c == 0x2029 || c == 0x202f || c == 0x205f || c == 0x3000;
}
inline bool valid_text(const std::string &s) {
    if (s.size() > max_text) return false;
    bool content = false;
    for (size_t p = 0; p < s.size();) {
        const auto c = scalar(s, p);
        if (c == 0) return false;
        content |= !space(c);
    }
    return content;
}
inline void utf8(std::string &s, uint32_t c) {
    if (c < 0x80) s += static_cast<char>(c);
    else if (c < 0x800) { s += static_cast<char>(0xc0 | (c >> 6)); s += static_cast<char>(0x80 | (c & 63)); }
    else if (c < 0x10000) { s += static_cast<char>(0xe0 | (c >> 12)); s += static_cast<char>(0x80 | ((c >> 6) & 63)); s += static_cast<char>(0x80 | (c & 63)); }
    else { s += static_cast<char>(0xf0 | (c >> 18)); s += static_cast<char>(0x80 | ((c >> 12) & 63)); s += static_cast<char>(0x80 | ((c >> 6) & 63)); s += static_cast<char>(0x80 | (c & 63)); }
}

class parser {
    const std::string &s;
    size_t p = 0;
    void ws() { while (p < s.size() && (s[p] == ' ' || s[p] == '\t' || s[p] == '\r')) ++p; }
    void token(char c) { ws(); if (p == s.size() || s[p++] != c) throw std::runtime_error("invalid request"); }
    uint32_t hex() {
        uint32_t n = 0;
        for (int i = 0; i < 4; ++i) {
            if (p == s.size()) throw std::runtime_error("invalid JSON escape");
            unsigned char c = s[p++]; unsigned v;
            if (c >= '0' && c <= '9') v = c - '0';
            else if (c >= 'a' && c <= 'f') v = c - 'a' + 10;
            else if (c >= 'A' && c <= 'F') v = c - 'A' + 10;
            else throw std::runtime_error("invalid JSON escape");
            n = n * 16 + v;
        }
        return n;
    }
    std::string string(size_t limit) {
        token('"'); std::string out;
        while (p < s.size()) {
            const auto c = static_cast<unsigned char>(s[p]);
            if (c == '"') { ++p; return out; }
            if (c < 32) throw std::runtime_error("invalid JSON string");
            if (c != '\\') {
                const auto start = p; scalar(s, p); out.append(s, start, p - start);
            } else {
                if (++p == s.size()) throw std::runtime_error("invalid JSON escape");
                switch (s[p++]) {
                case '"': out += '"'; break;
                case '\\': out += '\\'; break;
                case '/': out += '/'; break;
                case 'b': out += '\b'; break;
                case 'f': out += '\f'; break;
                case 'n': out += '\n'; break;
                case 'r': out += '\r'; break;
                case 't': out += '\t'; break;
                case 'u': {
                    uint32_t value = hex();
                    if (value >= 0xd800 && value <= 0xdbff) {
                        if (p + 2 > s.size() || s[p] != '\\' || s[p+1] != 'u') throw std::runtime_error("invalid JSON surrogate");
                        p += 2; const auto low = hex();
                        if (low < 0xdc00 || low > 0xdfff) throw std::runtime_error("invalid JSON surrogate");
                        value = 0x10000 + ((value - 0xd800) << 10) + low - 0xdc00;
                    } else if (value >= 0xdc00 && value <= 0xdfff) throw std::runtime_error("invalid JSON surrogate");
                    utf8(out, value); break;
                }
                default: throw std::runtime_error("invalid JSON escape");
                }
            }
            if (out.size() > limit) throw std::runtime_error("request field too large");
        }
        throw std::runtime_error("truncated JSON string");
    }
public:
    explicit parser(const std::string &input) : s(input) {}
    request parse() {
        if (s.size() >= max_header) throw std::runtime_error("request header too large");
        request r; unsigned fields = 0; token('{');
        for (;;) {
            const auto key = string(4); token(':');
            if (key == "id" && !(fields & 1)) { fields |= 1; r.id = string(32); }
            else if (key == "text" && !(fields & 2)) { fields |= 2; r.text = string(max_text); }
            else throw std::runtime_error("unknown or duplicate request field");
            ws(); if (p < s.size() && s[p] == ',') { ++p; continue; }
            token('}'); ws();
            if (p != s.size() || fields != 3 || r.id.empty() || !valid_text(r.text)) throw std::runtime_error("invalid request");
            return r;
        }
    }
};
inline bool header(std::istream &in, std::string &line) {
    wipe(line); char c;
    while (in.get(c)) {
        if (c == '\n') return true;
        if (line.size() >= max_header - 1) throw std::runtime_error("request header too large");
        line += c;
    }
    if (!line.empty() || !in.eof()) throw std::runtime_error("truncated request header");
    return false;
}
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
