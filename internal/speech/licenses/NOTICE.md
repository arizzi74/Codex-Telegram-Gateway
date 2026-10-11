# Local speech engine and model attribution

This application uses the CPU-only Pocket TTS implementation in
[Golem](https://github.com/ThiraSoft/golem), version **v0.45.2**, commit
`b92326c05f7185437e3e216acb8f98a52b82eceb`.
Copyright (c) 2026 Ricardo Carmona. The full MIT license is included in
`Golem-MIT.txt`.

The downloaded models, tokenizers, and precomputed stock voice embeddings are
provided by **Kyutai** in
[pocket-tts-without-voice-cloning](https://huggingface.co/kyutai/pocket-tts-without-voice-cloning),
immutable revision `1e08e6a23401048648a9fdcfde2f89348215c2a7`.
The repository identifies these assets as licensed under
[Creative Commons Attribution 4.0 International](https://creativecommons.org/licenses/by/4.0/).
The full terms are included in `CC-BY-4.0.txt`.

The application selects the six-layer English and Italian models and these
stock voices:

- **English — Alba**, voice performed by Alba MacKenna. The upstream
  [voice catalog](https://huggingface.co/kyutai/tts-voices#alba-mackenna)
  attributes Alba MacKenna's recordings and publishes them under CC BY 4.0.
- **Italian — Giovanni**, the stock voice embedding published by Kyutai.
  The upstream Pocket TTS catalog associates it with
  `common_voice_it_36520747-enhanced-v2.mp3`.

The downloaded binary assets are unmodified. `voice.safetensors` is a local
filename for the upstream `embeddings/alba.safetensors` or
`embeddings/giovanni.safetensors`; no voice cloning model or source recording
is downloaded. The application performs local inference using the selected
stock voice. Its optional generated audio replies are synthesized speech.

Assets are downloaded only when audio replies are prepared, over HTTPS, and
checked against fixed sizes and SHA-256 hashes in `internal/speech/models.go`.
English requires 225,283,535 bytes; Italian requires 223,220,709 bytes;
automatic language selection prepares both (448,504,244 bytes total).

These notices and license texts are embedded in the gateway binary and copied
beside its managed speech models during preparation. They are also distributed
with the application.
