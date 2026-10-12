# Resident Qwen3-TTS CPU helper

`codex-qwen-tts` embeds llama.cpp **b11560**, immutable commit
`b13a93a3d5fa44b57899d86467a4de463e7415a7`:
https://github.com/ggml-org/llama.cpp/tree/b13a93a3d5fa44b57899d86467a4de463e7415a7.
The build verifies the source archive SHA256 in `source.env` before extraction.
`threads.patch` propagates the selected CPU thread count into the Qwen speaker
mel preprocessor; the helper also sets the backbone and every mtmd graph's
thread count. No other upstream behavior is patched.

The executable contains llama.cpp and ggml (MIT), nlohmann/json (MIT),
miniaudio (MIT-0), stb_image (MIT), xxHash (BSD-2-Clause), rotate-bits (MIT),
and the YaRN rotary-embedding implementation by Jeffrey Quesnelle and Bowen
Peng (MIT).
Their notices are in `licenses/`. The included SHA1 implementation is
Steve Reid's public-domain implementation; SHA256 is Igor Pavlov's
public-domain implementation. The helper builds with subprocess spawning,
video, HTTP, GPU, dynamic backend loading, and OpenMP disabled.

The optional model pair comes from **ggml-org/Qwen3-TTS-12Hz-1.7B-Base-GGUF**,
immutable revision `ca27d74bc954b73dadab5b71ca265d87fc861a7c`:
https://huggingface.co/ggml-org/Qwen3-TTS-12Hz-1.7B-Base-GGUF/tree/ca27d74bc954b73dadab5b71ca265d87fc861a7c.
These are GGUF conversions and quantizations of **Qwen/Qwen3-TTS-12Hz-1.7B-Base**,
revision `fd4b254389122332181a7c3db7f27e918eec64e3`:
https://huggingface.co/Qwen/Qwen3-TTS-12Hz-1.7B-Base/tree/fd4b254389122332181a7c3db7f27e918eec64e3.
Qwen's model weights use Apache-2.0 (`licenses/Qwen-Apache-2.0.txt`). The exact
filenames, sizes and SHA256 hashes are in `source.env`. Weights are downloaded
separately into private storage and are not included in helper archives.

The embedded `stock-giovanni.wav` is **synthetic speech**, generated on
2026-10-12 with Golem v0.45.2 and Kyutai's Italian Pocket TTS model and
published Giovanni stock voice. Golem is by Ricardo Carmona (MIT;
`licenses/Golem-MIT.txt`). The model and stock embedding were provided by
**Kyutai** under CC BY 4.0 (`licenses/CC-BY-4.0.txt`) in
https://huggingface.co/kyutai/pocket-tts-without-voice-cloning/tree/1e08e6a23401048648a9fdcfde2f89348215c2a7.
The stock embedding is `languages/italian/embeddings/giovanni.safetensors`;
Kyutai's catalog associates Giovanni with `common_voice_it_36520747-enhanced-v2.mp3`.
The embedded audio is an adaptation produced by speech synthesis from these
assets. No person's uploaded voice recording was used. The speaker reference
contains: "Buongiorno, questa è una breve prova di una voce sintetica italiana.
Oggi il cielo è sereno e possiamo cominciare con calma."
It is mono 24 kHz PCM16, 8.8 seconds. Its SHA256 is pinned in `source.env`.
Qwen conditions on its speaker embedding; the transcript is not a model input.
This fixed reference is used for both supported languages. The product voice
is a synthesized stock voice, and must not be described as a human recording.

Linux helpers link musl 1.2.5 and GCC 14.2 runtime libraries statically, using
the pinned Alpine 3.22.2 platform images in `scripts/build-qwen-tts-linux.sh`.
The musl copyright and license and GCC GPLv3 plus Runtime Library Exception
are reproduced in `licenses/`. Sources are available at
https://musl.libc.org/releases/musl-1.2.5.tar.gz and
https://gcc.gnu.org/pub/gcc/releases/gcc-14.2.0/gcc-14.2.0.tar.xz.
Darwin helpers use Apple's system C/C++ libraries. Both architectures include
a portable CPU baseline: x86-64 or armv8-a, without requiring a GPU. A second
static helper, `lib/codex-qwen-tts-optimized`, uses optional CPU instructions.
The gateway selects it only after checking CPU and OS support. ARM64 uses
exactly `armv8-a+dotprod+fp16` and requires ASIMDDP, ASIMDHP and FPHP; it does
not require I8MM, SVE or SME. AMD64 enables SSE4.2, AVX, AVX2, BMI2, FMA and
F16C, and requires the corresponding OS-aware feature checks. The stock voice,
protocol, model filenames, and CLI are identical in both binaries.

Run `scripts/build-qwen-tts-linux.sh amd64|arm64`, or on a matching Mac run
`scripts/build-qwen-tts.sh`. Set `VERSION` for the gateway release and optionally
`BUILD_JOBS` (default 2), `QWEN_TTS_OUTPUT_DIR` (default `dist/qwen-tts`),
`QWEN_TTS_SOURCE_ARCHIVE` (a local copy of the verified source archive), or
`QWEN_TTS_MODEL_DIR` (run two real requests after building).
Output is a staging directory named `OS-ARCH` and
`codex-qwen-tts-OS-ARCH.tar.gz`, with `codex-qwen-tts`,
`lib/codex-qwen-tts-optimized`, notices, licenses,
`source.env` and an inner `SHA256SUMS` covering every other regular file.
An independent smoke test is available as
`python3 native/qwen-tts/smoke.py --helper PATH --dir MODEL_DIR`.
Python is used only by that build-time test. Inference requires neither Python
nor FFmpeg and does not read or write user text/audio files.

CLI: `codex-qwen-tts --dir ABSOLUTE_ENGINE_DIR --language italian|english
--threads 1..32`. Only the two pinned model filenames are opened under that
directory; the protocol accepts no path, voice, model, or inference options.
The Go manager verifies the assets before starting the helper. Models and
contexts remain resident while the helper processes requests sequentially.

Protocol 1 starts with `{"type":"ready","protocol":1,"sample_rate":24000}`.
Each bounded JSON line contains only `id` (1-32 bytes) and UTF-8 `text`
(1-32768 bytes, nonblank, without NUL). Success is a JSON line with `id`,
`sample_count`, `sample_rate:24000`, followed immediately by that many mono
float32 little-endian PCM samples. Failure is `{"id":"...","error":"synthesis_failed"}`.
Standard output contains only protocol traffic; diagnostics go to stderr.
Malformed requests terminate the process. EOF between requests exits cleanly.
Text is split on sentence/word boundaries into bounded token segments without
dropping content. Each segment clears the model KV cache and generation state.
Output is finite, clipped to [-1,1], bounded to ten minutes, and returned only
when every segment reaches end-of-speech before its frame/deadline bound.

Sampling follows Qwen's published backbone defaults (temperature 0.9, top-k
50, top-p 1.0 and repetition penalty 1.05). The pinned upstream code predictor
currently samples at effective temperature 1.0; its temperature 0.9 input is
not implemented. Voice/timing and long-text pronunciation remain model-dependent.
