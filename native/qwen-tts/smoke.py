#!/usr/bin/env python3
"""Build-time resident-helper smoke test; requests and PCM remain in memory."""
import argparse
import array
import json
import math
import pathlib
import resource
import subprocess
import sys
import tempfile
import time

parser = argparse.ArgumentParser()
parser.add_argument("--helper", required=True)
parser.add_argument("--dir", required=True)
parser.add_argument("--language", choices=("italian", "english"), default="italian")
parser.add_argument("--threads", type=int, default=2)
args = parser.parse_args()
texts = (["Ciao, questa è una breve prova.", "Il secondo messaggio usa lo stesso modello."]
         if args.language == "italian" else ["Hello, this is a short test.", "The second message uses the same model."])
started = time.monotonic()
durations = []
synthesis_seconds = []
with tempfile.TemporaryFile() as diagnostics:
    process = subprocess.Popen([str(pathlib.Path(args.helper).resolve()), "--dir", str(pathlib.Path(args.dir).resolve()),
                                "--language", args.language, "--threads", str(args.threads)],
                               stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=diagnostics)
    try:
        ready = json.loads(process.stdout.readline())
        assert ready == {"type": "ready", "protocol": 1, "sample_rate": 24000}, ready
        load_seconds = time.monotonic() - started
        for index, text in enumerate(texts):
            generation_started = time.monotonic()
            identifier = str(index + 1)
            process.stdin.write((json.dumps({"id": identifier, "text": text}) + "\n").encode())
            process.stdin.flush()
            header = json.loads(process.stdout.readline())
            assert header.get("id") == identifier and not header.get("error"), header
            assert header["sample_rate"] == 24000 and 0 < header["sample_count"] <= 24000 * 600, header
            size = header["sample_count"] * 4
            raw = bytearray()
            while len(raw) < size:
                part = process.stdout.read(min(32768, size - len(raw)))
                assert part, "truncated PCM"
                raw.extend(part)
            pcm = array.array("f")
            pcm.frombytes(raw)
            if sys.byteorder != "little":
                pcm.byteswap()
            assert all(math.isfinite(value) and -1 <= value <= 1 for value in pcm)
            assert math.sqrt(sum(value * value for value in pcm) / len(pcm)) > 0.001, "silent PCM"
            durations.append(len(pcm) / 24000)
            synthesis_seconds.append(round(time.monotonic() - generation_started, 3))
        process.stdin.close()
        assert process.wait(timeout=30) == 0
        assert not process.stdout.read(), "unexpected protocol output"
    finally:
        if process.poll() is None:
            process.kill()
            process.wait()
print(json.dumps({"requests": len(texts), "sample_rate": 24000, "audio_seconds": durations,
                  "synthesis_seconds": synthesis_seconds, "load_seconds": round(load_seconds, 3),
                  "total_seconds": round(time.monotonic() - started, 3),
                  "max_rss_mib": round(resource.getrusage(resource.RUSAGE_CHILDREN).ru_maxrss / (1024 * 1024 if sys.platform == "darwin" else 1024), 1)}))
