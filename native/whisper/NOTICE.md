# Local speech recognition notices and source

`codex-whisper` embeds the CPU backend of whisper.cpp **v1.8.7**,
commit `48f628a84833905ee4a0658ee6d4a5c915ce1997`.
The upstream source is available at
https://github.com/ggml-org/whisper.cpp/tree/48f628a84833905ee4a0658ee6d4a5c915ce1997
and its immutable source archive is retrieved by `scripts/build-whisper.sh`.
The archive SHA256 is recorded in `source.env` and verified before compilation.
whisper.cpp and its included ggml CPU implementation use the MIT license;
see `licenses/whisper.cpp-MIT.txt`.

The optional `ggml-base.bin` and `ggml-small.bin` models contain OpenAI's
multilingual Whisper base and small weights, converted by the whisper.cpp
project. Model weights and code use the
MIT license; see `licenses/Whisper-model-MIT.txt` and
https://github.com/openai/whisper/blob/main/LICENSE.
The converted models come from the immutable Hugging Face revision
`5359861c739e955e79d9a303bcbc70fb988958b1` of `ggerganov/whisper.cpp`.
Their exact sizes and SHA256 hashes are pinned in `source.env` and the native Go manager.
No model weights are bundled in release archives.

The Go gateway decodes Opus using Pion Opus, pinned to source commit
`44637de087b3` (`v0.1.1-0.20261005072002-44637de087b3`). It uses the MIT
license, reproduced in `licenses/Pion-Opus-MIT.txt`. This decoder is linked
into the pure Go gateway and adds no runtime library requirement.

Static Linux helpers also contain musl libc and GCC's libgcc/libstdc++ runtime
libraries. Alpine 3.22.2 platform images are pinned by digest in the build
script. musl 1.2.5 copyright and permissive license notices are reproduced in
`licenses/musl-COPYRIGHT.txt`; source is available from
https://musl.libc.org/releases/musl-1.2.5.tar.gz.
GCC 14.2 runtime libraries use GPL version 3 with the GCC Runtime Library
Exception, whose texts are reproduced in `licenses/GCC-GPL-3.0.txt` and
`licenses/GCC-Runtime-Library-Exception.txt`. GCC source is available from
https://gcc.gnu.org/pub/gcc/releases/gcc-14.2.0/gcc-14.2.0.tar.xz.
The standard GCC compilation and linking process used here qualifies for
that exception. Darwin helpers instead use Apple's supplied system libraries.

The helper receives only 16 kHz mono float32 little-endian PCM. Audio file
decoding occurs in the Go gateway. It loads one model once and processes
one framed request at a time, without carrying previous transcript context.
Protocol 1 uses a bounded JSON line followed by `sample_count * 4` PCM bytes.
Standard output contains only protocol JSON; native diagnostics use stderr.
EOF closes the model and exits cleanly. Invalid framing ends the process.
Inference uses the CPU, without a server, GPU, FFmpeg, or shared Linux libraries.

Build Linux artifacts with `scripts/build-whisper-linux.sh amd64|arm64`
(Docker plus QEMU is required when the architecture differs), and Darwin
artifacts with `scripts/build-whisper.sh` on the matching Mac architecture.
Set `VERSION` to the gateway release version. `WHISPER_OUTPUT_DIR` defaults to
`dist/whisper`. The reusable GitHub workflow builds all four targets, runs
the parser checks and two real speech transcriptions on each target, and
supplies these helpers to the release manifest and provenance pipeline.
For local release packaging, set `WHISPER_DIST_DIR` to that artifact directory.
