#include "protocol.h"
#include <iostream>

int main() {
    const auto r = protocol::parser("{\"language\":\"auto\", \"id\":\"job-1\",\"sample_count\":9600000}").parse();
    if (r.id != "job-1" || r.samples != protocol::max_samples) return 1;
    for (const auto &bad : {"{}", "{\"id\":\"a\",\"sample_count\":0,\"language\":\"auto\"}",
        "{\"id\":\"a\",\"sample_count\":9600001,\"language\":\"auto\"}",
        "{\"id\":\"a\",\"sample_count\":01,\"language\":\"auto\"}",
        "{\"id\":\"a\",\"id\":\"b\",\"sample_count\":1,\"language\":\"auto\"}",
        "{\"id\":\"a\",\"sample_count\":1.5,\"language\":\"auto\"}",
        "{\"id\":\"a\",\"sample_count\":1,\"language\":\"auto\"}x"}) {
        try { protocol::parser(bad).parse(); std::cerr << "accepted invalid header\n"; return 1; }
        catch (const std::runtime_error &) {}
    }
    if (protocol::quote("a\n\"b\\") != "\"a\\u000a\\\"b\\\\\"") return 1;
    return 0;
}
