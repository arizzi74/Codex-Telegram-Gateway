#!/usr/bin/env python3
"""Synthetic public-domain audio matching Telegram iOS's no-EOS Ogg shape.
Uses system libopus only to encode deterministic synthesized PCM and generate a
16 kHz mono independent decoder oracle; page mux/CRC is pure Python.
Telegram writer settings: mono 48 kHz, 960-sample frames, APPLICATION_AUDIO,
30*1024 bit/s, max packet 383 bytes, OpusTags padded by 512 to 764 bytes.
Telegram nil final frame flushes completed packets without adding an EOS packet.
"""
import ctypes as C
import ctypes.util
import math
from pathlib import Path
import struct

OUT=Path(__file__).resolve().parent
opus=C.CDLL(ctypes.util.find_library('opus'))
opus.opus_get_version_string.restype=C.c_char_p
opus.opus_encoder_create.argtypes=[C.c_int,C.c_int,C.c_int,C.POINTER(C.c_int)]
opus.opus_encoder_create.restype=C.c_void_p
opus.opus_encoder_ctl.argtypes=[C.c_void_p,C.c_int]
opus.opus_encode.argtypes=[C.c_void_p,C.POINTER(C.c_int16),C.c_int,C.POINTER(C.c_ubyte),C.c_int]
opus.opus_encoder_destroy.argtypes=[C.c_void_p]
opus.opus_decoder_create.argtypes=[C.c_int,C.c_int,C.POINTER(C.c_int)]
opus.opus_decoder_create.restype=C.c_void_p
opus.opus_decode_float.argtypes=[C.c_void_p,C.POINTER(C.c_ubyte),C.c_int,C.POINTER(C.c_float),C.c_int,C.c_int]
opus.opus_decoder_destroy.argtypes=[C.c_void_p]

err=C.c_int()
enc=opus.opus_encoder_create(48000,1,2049,C.byref(err))
assert err.value==0
assert opus.opus_encoder_ctl(enc,4002,C.c_int(30*1024))==0
lookahead=C.c_int()
assert opus.opus_encoder_ctl(enc,4027,C.byref(lookahead))==0
packets=[]
for frame in range(120):
 pcm=(C.c_int16*960)(*[int(6500*math.sin(2*math.pi*440*(frame*960+i)/48000)+3200*math.sin(2*math.pi*173*(frame*960+i)/48000)) for i in range(960)])
 out=(C.c_ubyte*383)()
 n=opus.opus_encode(enc,pcm,960,out,len(out))
 assert n>0,n
 packets.append(bytes(out[:n]))
opus.opus_encoder_destroy(enc)

def crc(data):
 v=0
 for b in data:
  v^=b<<24
  for _ in range(8):v=((v<<1)^(0x04c11db7 if v&0x80000000 else 0))&0xffffffff
 return v

def page(flags,granule,sequence,packets):
 laces=[]
 for p in packets:laces.extend([255]*(len(p)//255)+[len(p)%255])
 assert len(laces)<=255
 header=bytearray(b'OggS'+bytes([0,flags])+struct.pack('<QIII',granule,42,sequence,0)+bytes([len(laces)])+bytes(laces))
 data=header+b''.join(packets)
 struct.pack_into('<I',data,22,crc(data))
 return data

head=b'OpusHead'+struct.pack('<BBHIhB',1,1,lookahead.value,48000,0,0)
vendor=opus.opus_get_version_string()
tags=b'OpusTags'+struct.pack('<I',len(vendor))+vendor+struct.pack('<I',0)
padded=(len(tags)+512+255)//255*255-1
tags+=bytes(padded-len(tags))
pages=[page(2,0,0,[head]),page(0,0,1,[tags])]
for offset in range(0,len(packets),50):
 group=packets[offset:offset+50]
 pages.append(page(0,(offset+len(group))*960,len(pages),group))
(OUT/'telegram-ios-no-eos.ogg').write_bytes(b''.join(pages))

dec=opus.opus_decoder_create(16000,1,C.byref(err));assert err.value==0
samples=[]
for p in packets:
 data=(C.c_ubyte*len(p)).from_buffer_copy(p)
 out=(C.c_float*1920)()
 n=opus.opus_decode_float(dec,data,len(p),out,len(out),0);assert n>0,n
 samples.extend(out[:n])
opus.opus_decoder_destroy(dec)
samples=samples[lookahead.value//3:]
(OUT/'telegram-ios-no-eos.f32le').write_bytes(struct.pack('<'+'f'*len(samples),*samples))
print('libopus',vendor.decode(),'preskip',lookahead.value,'audio_packets',len(packets),'tags_bytes',len(tags),'final_granule',len(packets)*960,'expected_samples',len(samples))
for p in OUT.iterdir():
 if p.suffix in ('.ogg','.f32le'):print(p.name,p.stat().st_size)
