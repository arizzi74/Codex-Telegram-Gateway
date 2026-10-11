#!/usr/bin/env python3
"""Rebuild fixture audio and independent libopus goldens; not a runtime tool."""
import ctypes
import ctypes.util
from pathlib import Path
import shutil
import struct
import subprocess
import sys


def packets(data):
    offset, partial, output = 0, bytearray(), []
    final = 0
    while offset < len(data):
        count = data[offset + 26]
        lacing = data[offset + 27:offset + 27 + count]
        payload = offset + 27 + count
        final = struct.unpack_from("<Q", data, offset + 6)[0]
        for size in lacing:
            partial.extend(data[payload:payload + size])
            payload += size
            if size < 255:
                output.append(bytes(partial))
                partial.clear()
        offset = payload
    assert not partial
    return output, final


def reference(lib, path):
    data, final = packets(path.read_bytes())
    pre_skip = struct.unpack_from("<H", data[0], 10)[0]
    gain = struct.unpack_from("<h", data[0], 16)[0]
    error = ctypes.c_int()
    decoder = lib.opus_decoder_create(16000, 1, ctypes.byref(error))
    assert decoder and error.value == 0
    samples = []
    try:
        for packet in data[2:]:
            output = (ctypes.c_float * 1920)()
            count = lib.opus_decode_float(decoder, packet, len(packet), output, 1920, 0)
            assert count > 0, (path, count)
            samples.extend(output[:count])
    finally:
        lib.opus_decoder_destroy(decoder)
    samples = samples[pre_skip // 3:pre_skip // 3 + (final - pre_skip) // 3]
    scale = 10 ** (gain / (20 * 256))
    path.with_suffix(".f32le").write_bytes(
        struct.pack("<" + "f" * len(samples), *(max(-1, min(1, x * scale)) for x in samples))
    )


def main():
    upstream = Path(sys.argv[1])
    output = Path(__file__).resolve().parent
    shutil.copyfile(upstream / "testdata/tiny.ogg", output / "tiny.ogg")
    for name, source, rate, bitrate, application in [
        ("silk-stereo", "reference-silk-rfc8251-stereo.wav", "16000", "24k", "voip"),
        ("hybrid-stereo", "reference-hybrid-rfc8251-stereo.wav", "48000", "32k", "voip"),
        ("celt-mono", "reference-celt-periodic-mono.wav", "48000", "128k", "lowdelay"),
    ]:
        subprocess.run([
            "ffmpeg", "-nostdin", "-v", "error", "-y", "-i",
            str(upstream / "testdata/short-plc/audio" / source), "-c:a", "libopus",
            "-ar", rate, "-b:a", bitrate, "-application", application,
            str(output / (name + ".ogg")),
        ], check=True)
    lib = ctypes.CDLL(ctypes.util.find_library("opus"))
    lib.opus_decoder_create.argtypes = [ctypes.c_int, ctypes.c_int, ctypes.POINTER(ctypes.c_int)]
    lib.opus_decoder_create.restype = ctypes.c_void_p
    lib.opus_decode_float.argtypes = [ctypes.c_void_p, ctypes.c_void_p, ctypes.c_int,
                                    ctypes.POINTER(ctypes.c_float), ctypes.c_int, ctypes.c_int]
    lib.opus_decoder_destroy.argtypes = [ctypes.c_void_p]
    lib.opus_get_version_string.restype = ctypes.c_char_p
    print("Reference decoder:", lib.opus_get_version_string().decode())
    for path in output.glob("*.ogg"):
        reference(lib, path)


if __name__ == "__main__":
    main()
