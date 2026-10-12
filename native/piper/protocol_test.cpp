#include "protocol.h"
#include <iostream>
#include <sstream>
#include <vector>

int main() {
    int failures = 0;
    const auto good = protocol::parser("{\"text\":\"Caff\\u00e8: \\ud83d\\ude80\\n\\\"ok\\\"\",\"id\":\"a\"}").parse();
    if (good.id != "a" || good.text != "Caffè: 🚀\n\"ok\"") ++failures;
    const std::vector<std::string> bad = {
        "{}", "{\"id\":\"a\"}", "{\"id\":\"a\",\"text\":\" \"}", "{\"id\":\"\",\"text\":\"ok\"}",
        "{\"id\":\"a\",\"text\":\"ok\",\"id\":\"b\"}", "{\"id\":\"a\",\"text\":\"ok\",\"extra\":\"b\"}",
        "{\"id\":\"a\",\"text\":\"\\u0000\"}", "{\"id\":\"a\",\"text\":\"\\ud800\"}",
        "{\"id\":\"a\",\"text\":\"\\udc00\"}", "{\"id\":\"a\",\"text\":\"\\ud800\\u0041\"}",
        "{\"id\":\"a\",\"text\":\"\\u2000\\u3000\"}", "{\"id\":\"a\",\"text\":\"ok\"} {}",
        "{\"id\":\"a\",\"text\":\"\xc0\xaf\"}", "{\"id\":\"a\",\"text\":\"\xed\xa0\x80\"}",
        "{\"id\":\"" + std::string(33, 'a') + "\",\"text\":\"ok\"}",
        "{\"id\":\"a\",\"text\":\"" + std::string(protocol::max_text + 1, 'a') + "\"}",
        std::string(protocol::max_header, ' ')
    };
    for (const auto &s : bad) { try { protocol::parser(s).parse(); ++failures; } catch (const std::exception &) {} }
    if (protocol::parser("{\"id\":\"a\",\"text\":\"" + std::string(protocol::max_text, 'a') + "\"}").parse().text.size() != protocol::max_text) ++failures;
    for (const auto &s : std::vector<std::string>{"truncated", std::string(protocol::max_header, ' ') + "\n"}) {
        try { std::istringstream in(s); std::string line; protocol::header(in, line); ++failures; } catch (const std::exception &) {}
    }
    std::string line; std::istringstream empty;
    if (protocol::header(empty, line)) ++failures;
    if (failures) std::cerr << failures << " protocol checks failed\n";
    return failures ? 1 : 0;
}
