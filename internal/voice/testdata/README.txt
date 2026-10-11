Audio fixture provenance

Upstream: github.com/pion/opus at revision
44637de087b3f506170ab738ed96ac1be338006b (October 5, 2026).

tiny.ogg is the unmodified upstream testdata/tiny.ogg. The three longer
fixtures are encoded from upstream testdata/short-plc/audio/ files:

  silk-stereo.ogg: reference-silk-rfc8251-stereo.wav, libopus 24kbps,
                   16kHz input, voip application (SILK wideband stereo).
  hybrid-stereo.ogg: reference-hybrid-rfc8251-stereo.wav, libopus 32kbps,
                     48kHz input, voip application (hybrid fullband stereo).
  celt-mono.ogg: reference-celt-periodic-mono.wav, libopus 128kbps,
                 48kHz input, lowdelay application (CELT fullband mono).

All four upstream source fixtures carry:
SPDX-FileCopyrightText: 2026 The Pion community <https://pion.ly>
SPDX-License-Identifier: MIT
The included LICENSE.txt applies to the copied and derived fixtures.

The .f32le files are independent golden reference PCM decoded by the system
libopus 1.3.1, directly configured for 16000Hz mono, then pre-skipped and
trimmed using the Ogg granule position. They are not produced by Pion or
this package. TestDecodeGoldenLibopus compares duration and audible sample
values for SILK, hybrid, CELT, mono and stereo streams.

Reproduction (maintenance only):
  python3 internal/voice/testdata/generate.py /path/to/pion/opus/module

The generator uses FFmpeg to encode the derived Ogg files and libopus to
produce independent reference PCM. Neither tool is needed by the gateway
or the Go tests; decoding in the shipped gateway uses pure Go.
