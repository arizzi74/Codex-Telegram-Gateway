---
language:
- it
license: cc-by-nc-nd-4.0
pipeline_tag: text-to-speech
datasets:
- TigreGotico/tts-train-synthetic-miro_it-IT
base_model:
- OpenVoiceOS/pipertts_pt-PT_miro
tags:
- piper
- onnx
- vits
- text-to-speech
- it
---

# pipertts_it-IT_miro

This is a text-to-speech voice model for **Italian**. The voice is **Miro**, a male voice. It uses the Piper VITS architecture, exported to ONNX for CPU inference. It was fine-tuned from the base voice `pipertts_pt-PT_miro`, the closest available language for this voice.

## How to use

This is a [Piper](https://github.com/OHF-Voice/piper1-gpl) VITS voice, in
ONNX format. Use it with any Piper-compatible runtime, or with
[ovos-tts-plugin-piper](https://github.com/OpenVoiceOS/ovos-tts-plugin-piper)
in OVOS:

```json
{
  "tts": {
    "module": "ovos-tts-plugin-piper",
    "ovos-tts-plugin-piper": {
      "voice": "OpenVoiceOS/pipertts_it-IT_miro"
    }
  }
}
```

Command line, with the `piper` package installed:

```bash
echo "Buongiorno, come stai?" | piper --model OpenVoiceOS/pipertts_it-IT_miro --output_file output.wav
```

## Training data

Trained on [TigreGotico/tts-train-synthetic-miro_it-IT](https://huggingface.co/datasets/TigreGotico/tts-train-synthetic-miro_it-IT).

## Related links

- Training-data collection: https://huggingface.co/collections/TigreGotico/synthetic-tts-datasets
- phoonnx: https://github.com/TigreGotico/phoonnx
- voiceclonnx: https://github.com/TigreGotico/voiceclonnx
- TigreGotico: https://tigregotico.pt

## Ownership and licensing

Miro and Dii are the recorded voices of two real people. The voices, the
training data, and this model belong to TigreGotico Lda
(https://tigregotico.pt).

This model is licensed under Creative Commons
Attribution-NonCommercial-NoDerivatives 4.0 (CC BY-NC-ND 4.0). You may use
and share this model for non-commercial purposes only. Give attribution to
TigreGotico Lda. Do not modify, adapt, or build new models from this model.
Do not use the voice identity to create derivative voices.

For commercial use, for derivative models, or for any other license of the
Miro or Dii voice identity, contact TigreGotico Lda.

The training checkpoint is kept out of this repository so a download is the
model and not the training state. It is available on request.
