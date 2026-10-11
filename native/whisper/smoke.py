#!/usr/bin/env python3
"""Verify real multilingual Whisper inference and warm protocol on a native host."""
import array
import hashlib
import json
import os
from pathlib import Path
import struct
import subprocess
import sys
import tempfile
import time
import urllib.request
import wave

ROOT = Path(__file__).resolve().parents[2]
PIN = dict(line.split("=", 1) for line in (ROOT / "native/whisper/source.env").read_text().splitlines() if line and not line.startswith("#"))


def download(url, target, digest):
    if not target.exists():
        with urllib.request.urlopen(url, timeout=180) as source, target.open("wb") as dest:
            while data := source.read(1024 * 1024):
                dest.write(data)
    hash = hashlib.sha256()
    with target.open("rb") as source:
        while data := source.read(1024 * 1024):
            hash.update(data)
    if hash.hexdigest() != digest:
        raise RuntimeError("smoke fixture checksum mismatch")


def main():
    helper = Path(sys.argv[1]).resolve()
    model_name = os.environ.get("WHISPER_TEST_MODEL_NAME", "base")
    if model_name not in ("base", "small"):
        raise ValueError("WHISPER_TEST_MODEL_NAME must be base or small")
    threads = int(os.environ.get("WHISPER_TEST_THREADS", "2"))
    if not 1 <= threads <= 32:
        raise ValueError("WHISPER_TEST_THREADS must be between 1 and 32")
    pin_prefix = "WHISPER_MODEL_" if model_name == "base" else "WHISPER_SMALL_MODEL_"
    model_filename = "ggml-" + model_name + ".bin"
    with tempfile.TemporaryDirectory(prefix="codex-whisper-smoke-") as directory:
        tmp = Path(directory)
        model = Path(os.environ.get("WHISPER_TEST_MODEL", str(tmp / model_filename)))
        download("https://huggingface.co/ggerganov/whisper.cpp/resolve/" + PIN[pin_prefix + "COMMIT"] + "/" + model_filename + "?download=true", model, PIN[pin_prefix + "SHA256"])
        speech = tmp / "jfk.wav"
        download("https://raw.githubusercontent.com/ggml-org/whisper.cpp/" + PIN["WHISPER_COMMIT"] + "/samples/jfk.wav", speech, "59dfb9a4acb36fe2a2affc14bacbee2920ff435cb13cc314a08c13f66ba7860e")
        with wave.open(str(speech)) as source:
            assert source.getnchannels() == 1 and source.getframerate() == 16000 and source.getsampwidth() == 2
            pcm = [sample[0] / 32768.0 for sample in struct.iter_unpack("<h", source.readframes(source.getnframes()))]
        with (tmp / "native.log").open("wb") as log:
            started = time.monotonic()
            process = subprocess.Popen([str(helper), "--model", str(model), "--threads", str(threads)], stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=log)
            try:
                assert json.loads(process.stdout.readline()) == {"type": "ready", "protocol": 1}
                print("Cold model load: %.3fs" % (time.monotonic() - started), flush=True)
                pid = process.pid
                # Two independently framed speech jobs in the same process.
                for job, samples in enumerate((pcm, pcm[:16000 * 6])):
                    started = time.monotonic()
                    request_id = "smoke-" + str(job)
                    process.stdin.write((json.dumps({"id": request_id, "sample_count": len(samples), "language": "auto"}) + "\n").encode())
                    payload = array.array("f", samples)
                    if sys.byteorder != "little":
                        payload.byteswap()
                    process.stdin.write(payload.tobytes())
                    process.stdin.flush()
                    response = json.loads(process.stdout.readline())
                    assert response["id"] == request_id and response["language"] == "en" and not response.get("error"), response
                    assert ("country" if job == 0 else "americans") in response["text"].lower(), response
                    assert process.pid == pid and process.poll() is None
                    print("Warm speech job %d: %.3fs: %s" % (job + 1, time.monotonic() - started, response["text"].strip()), flush=True)
                if sys.platform.startswith("linux"):
                    def cpu_ticks():
                        fields = Path("/proc/%d/stat" % pid).read_text().rsplit(")", 1)[1].split()
                        return int(fields[11]) + int(fields[12])
                    before = cpu_ticks()
                    time.sleep(15)
                    cpu = (cpu_ticks() - before) / os.sysconf("SC_CLK_TCK")
                    rss = next(line for line in Path("/proc/%d/status" % pid).read_text().splitlines() if line.startswith("VmRSS:"))
                    print("Idle over 15s: %.3f CPU-seconds; %s" % (cpu, rss), flush=True)
                process.stdin.close()
                assert process.wait(timeout=20) == 0
                assert process.stdout.read() == b"", "stdout contains native diagnostics"
            finally:
                if process.poll() is None:
                    process.kill()
                    process.wait()
        # Invalid framing must fail before any PCM allocation or inference.
        bad = subprocess.run([str(helper), "--model", str(model)], input=b'{"id":"oversize","sample_count":9600001,"language":"auto"}\n', capture_output=True, timeout=120)
        assert bad.returncode != 0 and bad.stdout == b'{"type":"ready","protocol":1}\n'
        print("Oversized framing and graceful EOF passed.")


if __name__ == "__main__":
    main()
