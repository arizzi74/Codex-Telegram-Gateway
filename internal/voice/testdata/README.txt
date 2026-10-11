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

Telegram iOS EOF compatibility fixture

telegram-ios-no-eos.ogg contains synthesized tones, with no private recording
or third-party audio. It follows Telegram iOS's writer settings and page shape:
48kHz mono, 960-sample AUDIO frames, 30*1024bps, pre-skip312, padded OpusTags,
and complete audio pages ending at physical EOF without an Ogg EOS flag.
The .f32le golden was independently decoded by libopus1.3.1 at16kHz mono.
Reproduce with python3 internal/voice/testdata/generate-ios.py (system libopus).
The synthesized audio is dedicated to the public domain; the generator is
covered by the repository's license. The Go test creates an EOS-marked control
from the same public fixture and verifies identical PCM.

Primary-source producer behavior at Telegram-iOS revision
f1dd7a2dbd02cbbf513e75d5695d8d36d1cf5838:
https://github.com/TelegramMessenger/Telegram-iOS/blob/f1dd7a2dbd02cbbf513e75d5695d8d36d1cf5838/submodules/TelegramUI/Sources/ManagedAudioRecorder.swift#L696-L698
https://github.com/TelegramMessenger/Telegram-iOS/blob/f1dd7a2dbd02cbbf513e75d5695d8d36d1cf5838/submodules/OpusBinding/Sources/opusenc/opusenc.m#L592-L628
