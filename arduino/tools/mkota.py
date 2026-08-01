#!/usr/bin/env python3

# SPDX-License-Identifier: AGPL-3.0-or-later
# Copyright (C) 2026 Daniel Snider

"""Convert a compiled Arduino .bin to the .ota format the R4 WiFi OTAUpdate library
expects. Stdlib-only (zlib.crc32 == Arduino's crccheck Crc32). Format:
  [len u32 LE][crc32 u32 LE] over: [magic u32 LE][version 8B][payload]
"""
import sys, zlib

MAGIC = {"UNOR4WIFI": 0x23411002}   # = USB VID:PID 2341:1002

if len(sys.argv) != 4:
    sys.exit("usage: mkota.py BOARD in.bin out.ota")
board, ifile, ofile = sys.argv[1], sys.argv[2], sys.argv[3]
if board not in MAGIC:
    sys.exit(f"unsupported board {board}")

magic = MAGIC[board].to_bytes(4, "little")
version = bytes([0, 0, 0, 0, 0, 0, 0, 0x40])   # byte7=0x40 = LZSS-compressed payload flag
payload = open(ifile, "rb").read()
complete = magic + version + payload
crc = zlib.crc32(complete) & 0xFFFFFFFF
blob = len(complete).to_bytes(4, "little") + crc.to_bytes(4, "little") + complete
open(ofile, "wb").write(blob)
print(f"{ofile}: {len(blob)} bytes  (payload {len(payload)}, crc 0x{crc:08x})")
