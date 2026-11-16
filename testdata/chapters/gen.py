#!/usr/bin/env python3
"""Generate the chapter test fixture: a 2.4 s MP3 with two 1.2 s chapters.

Chapter 1 is a 440 Hz tone, chapter 2 an 880 Hz tone, so tests can tell the
chapters apart by the waveform. The ID3v2.4 tag carries a top-level ordered
CTOC and two CHAP frames (deliberately stored out of order).
"""
import math, struct, subprocess, sys, tempfile, os

def synchsafe(n):
    return bytes([(n >> 21) & 0x7f, (n >> 14) & 0x7f, (n >> 7) & 0x7f, n & 0x7f])

def frame(fid, body):
    return fid.encode() + synchsafe(len(body)) + b'\x00\x00' + body

def text(s):
    return bytes([3]) + s.encode('utf-8')

def chap(eid, start_ms, end_ms, title):
    body = eid.encode() + b'\x00'
    body += struct.pack('>II', start_ms, end_ms)
    body += b'\xff\xff\xff\xff\xff\xff\xff\xff'  # offsets: unknown
    body += frame('TIT2', text(title))
    return frame('CHAP', body)

def ctoc(eid, entries, title):
    body = eid.encode() + b'\x00'
    body += bytes([0x03, len(entries)])  # top-level + ordered
    for e in entries:
        body += e.encode() + b'\x00'
    body += frame('TIT2', text(title))
    return frame('CTOC', body)

def main(outdir):
    sr = 44100
    with tempfile.TemporaryDirectory() as tmp:
        wav = os.path.join(tmp, 'tone.wav')
        with open(wav, 'wb') as f:
            pass
        import wave
        with wave.open(wav, 'wb') as w:
            w.setnchannels(2); w.setsampwidth(2); w.setframerate(sr)
            frames = bytearray()
            for i in range(int(sr * 2.4)):
                f = 440.0 if i < int(sr * 1.2) else 880.0
                v = int(32767 * 0.6 * math.sin(2 * math.pi * f * i / sr))
                frames += struct.pack('<hh', v, v)
            w.writeframes(bytes(frames))
        base = os.path.join(tmp, 'base.mp3')
        subprocess.run(['ffmpeg', '-hide_banner', '-loglevel', 'error', '-i', wav,
                        '-codec:a', 'libmp3lame', '-b:a', '128k', '-ar', str(sr),
                        '-ac', '2', '-write_xing', '0', '-id3v2_version', '0',
                        base, '-y'], check=True)
        audio = open(base, 'rb').read()

    tag = frame('TIT2', text('Two chapter story'))
    tag += ctoc('toc', ['ch1', 'ch2'], 'Story')
    tag += chap('ch2', 1200, 2400, 'Chapter Two')
    tag += chap('ch1', 0, 1200, 'Chapter One')
    tag += b'\x00' * 32
    tag = b'ID3\x04\x00\x00' + synchsafe(len(tag)) + tag

    with open(os.path.join(outdir, 'two_chapters.mp3'), 'wb') as f:
        f.write(tag + audio)
    with open(os.path.join(outdir, 'two_chapters_noid3.mp3'), 'wb') as f:
        f.write(audio)

if __name__ == '__main__':
    main(sys.argv[1] if len(sys.argv) > 1 else '.')
